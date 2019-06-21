// Copyright 2015 Tigera Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package upgrade

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"time"

	"github.com/containernetworking/plugins/plugins/ipam/host-local/backend/disk"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/projectcalico/cni-plugin/internal/pkg/utils"
	v3 "github.com/projectcalico/libcalico-go/lib/apis/v3"
	bapi "github.com/projectcalico/libcalico-go/lib/backend/api"
	"github.com/projectcalico/libcalico-go/lib/backend/k8s"
	client "github.com/projectcalico/libcalico-go/lib/clientv3"
	"github.com/projectcalico/libcalico-go/lib/errors"
	"github.com/projectcalico/libcalico-go/lib/ipam"
	cnet "github.com/projectcalico/libcalico-go/lib/net"
	"github.com/projectcalico/libcalico-go/lib/options"
	corev1 "k8s.io/api/core/v1"
)

const (
	ipAllocPath = "/var/lib/cni/networks/k8s-pod-network"
)

var (
	binariesToDisable = []string{
		"/host/opt/cni/bin/calico",
		"/host/opt/cni/bin/host-local",
	}
)

type accessor interface {
	Backend() bapi.Client
}

func Migrate(ctxt context.Context, c client.Interface, nodename string) error {
	// k8sClient directly calls the k8s apiserver.
	k8sClient := c.(accessor).Backend().(*k8s.KubeClient).ClientSet

	// Check to see if the system is still using host-local
	// by checking the existence of the path.
	log.Info("checking host-local IPAM data dir dir existence...")
	if _, err := os.Stat(ipAllocPath); err != nil && os.IsNotExist(err) {
		log.Info("host-local IPAM data dir dir not found; no migration necessary, successfully exiting...")
		return nil
	}


	// Open k8s-pod-directory to check for emptiness.
	log.Info("checking if host-local IPAM data dir dir is empty...")
	ipamDir, err := os.Open(ipAllocPath)
	if err != nil {
		return fmt.Errorf("failed to open host-local IPAM data dir dir: %s", err)
	}

	// Check if the directory is empty.
	if _, err = ipamDir.Readdirnames(1); err != nil {
		if os.IsNotExist(err) || err == io.EOF {
			log.Info("host-local IPAM data dir empty; no migration necessary...")
			log.Info("removing host-local IPAM data directory...")
			if err = os.Remove(ipAllocPath); err != nil {
				log.WithError(err).Error("failed to remove host-local IPAM data dir directory")
				return err
			}
			log.Info("successfully removed host-local IPAM data directory!")
			return nil
		}
		if closeErr := ipamDir.Close(); closeErr != nil {
			return fmt.Errorf("failed to close host-local IPAM data dir directory on read failure: %s", err)
		}
		return fmt.Errorf("failed to read host-local IPAM data dir names: %s", err)
	}
	log.Info("host-local IPAM data dir is not empty, migrating...")

	// Close the host-local IPAM data directory file pointer.
	if closeErr := ipamDir.Close(); closeErr != nil {
		return fmt.Errorf("failed to close host-local IPAM data dir directory: %s", err)
	}

	// Disable cni by setting DatastoreReady to false.
	log.Info("setting datastore readiness to false")
	var clusterInfo *v3.ClusterInformation
	for i := uint(0); i < 5; i++ {
		clusterInfo, err = c.ClusterInformation().Get(ctxt, "default", options.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to fetch cluster information: %s", err)
		}
		if clusterInfo.Spec.DatastoreReady == nil || *clusterInfo.Spec.DatastoreReady {
			f := false
			clusterInfo.Spec.DatastoreReady = &f
			if clusterInfo, err = c.ClusterInformation().Update(ctxt, clusterInfo, options.SetOptions{}); err != nil {
				if _, ok := err.(errors.ErrorResourceUpdateConflict); ok {
					log.Info("Encountered update conflict, retrying...")
					time.Sleep(1 * time.Second)
					continue
				}
				return fmt.Errorf("failed to disable cluster: %s", err)
			}
			break
		}
	}
	log.Info("successfully set datastore readiness to false")

	// Also disable cni by deleting the binaries.
	log.Info("removing cni binaries...")
	for _, binary := range binariesToDisable {
		if err = os.Remove(binary); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove binary %s: %s", binary, err)
		}
		log.WithField("binary", binary).Info("successfully removed cni binary!")
	}
	time.Sleep(5 * time.Second)

	// Acquire a lock on the host-local cni backend. This serves as extra precaution
	// against racing with any remaining host-local processes which might be allocating
	// IP addresses.
	log.Info("acquiring lock on host-local IPAM")
	hostLocal, err := disk.New("", ipAllocPath)
	if err != nil {
		return fmt.Errorf("failed to initialize host-local IPAM: %s", err)
	}
	if err = hostLocal.Lock(); err != nil {
		return fmt.Errorf("failed to acquire lock on host-local IPAM: %s", err)
	}
	log.Info("successfully acquired lock on host-local IPAM")
	defer func() {
		// Release the lock on host local backend
		log.Info("releasing lock on host-local backend...")
		if err = hostLocal.Unlock(); err != nil {
			log.WithError(err).Error("failed to release lock on host local backend")
		} else {
			log.Info("successfully released lock on host-local backend!")
		}
	}()

	// Establishing a mapping of IP addresses to Pods on this node. We need this
	// to populate Calico IPAM's allocation attributes below.
	log.Info("mapping pod ips to pods...")
	podList, err := k8sClient.CoreV1().Pods("").List(metav1.ListOptions{
		FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodename),
	})
	if err != nil {
		return fmt.Errorf("failed to list pods: %s", err)
	}
	podIPMap := make(map[string]*corev1.Pod)
	for i := 0; i < len(podList.Items); i++ {
		pod := &podList.Items[i]
		log.WithFields(log.Fields{"pod": pod.Name, "IP": pod.Status.PodIP, "namespace": pod.Namespace}).Info("mapping in pod")
		podIPMap[pod.Status.PodIP] = pod
	}
	log.Info("successfully mapped pod ips to pods!")

	// Read in all the files in the host-local directory.
	log.Info("reading files from host-local IPAM data dir...")
	files, err := ioutil.ReadDir(ipAllocPath)
	if err != nil {
		return fmt.Errorf("failed to read path %s: %s", ipAllocPath, err)
	}

	// For each file, convert it into an IP allocation and then delete the file.
	for _, f := range files {
		logCtxt := log.WithField("file", f.Name())
		logCtxt.Info("processing file...")
		fname := path.Join(ipAllocPath, f.Name())

		// Delete and skip the last reserved IP.
		if strings.Contains(f.Name(), "last") {
			logCtxt.Info("skipping and removing last reserved ip file...")
			if err = os.Remove(fname); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("failed to remove file %s: %s", fname, err)
			}
			logCtxt.Info("removed last reserved ip file")
			continue
		}

		// Skip the lock
		if f.Name() == "lock" {
			logCtxt.Info("skipping the lock...")
			continue
		}

		// The name of the file is its IP address.
		ip, _, err := cnet.ParseCIDR(fmt.Sprintf("%s/32", f.Name()))
		if err != nil {
			return fmt.Errorf("failed to parse IP %s: %s", f.Name(), err)
		}

		// The contents are the container ID.
		b, err := ioutil.ReadFile(fname)
		if err != nil {
			return fmt.Errorf("failed to read file %s: %s", fname, err)
		}
		containerID := string(b)

		// Get the pod resource associated with the IP.
		pod, ok := podIPMap[f.Name()]
		if !ok {
			logCtxt.WithField("ip", f.Name()).Info("pod not found for IP, deleting and continuing")
			if err = os.Remove(fname); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("failed to remove file %s: %s", fname, err)
			}
			continue
		}

		// Store allocation to Calico datastore.
		handleID := utils.GetHandleID("k8s-pod-network", containerID, "")
		logCtxt.Info("assinging pod IP to Calico IPAM...")
		if err = c.IPAM().AssignIP(ctxt, ipam.AssignIPArgs{
			IP:       *ip,
			HandleID: &handleID,
			Hostname: nodename,
			Attrs: map[string]string{
				ipam.AttributeNode:      nodename,
				ipam.AttributePod:       pod.Name,
				ipam.AttributeNamespace: pod.Namespace,
			},
		}); err != nil {
			if _, ok := err.(errors.ErrorResourceAlreadyExists); !ok {
				return fmt.Errorf("failed to assign IP to calico backend: %s", err)
			}
			// Pod IP already assigned - likely failed to remove the file on the last attempt.
			// continue, but log a warning.
			logCtxt.Warn("pod IP already assigned, skipping")
		}
		logCtxt.Info("pod IP assigned in Calico IPAM")

		// Delete the file from the host-local directory.
		logCtxt.Info("removing host-local allocation file")
		if err = os.Remove(fname); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove file %s: %s", fname, err)
		}
		logCtxt.Info("successfully removed file")
	}

	// Release the lock.
	if err = hostLocal.Unlock(); err != nil {
		log.WithError(err).Error("failed to release lock on host local backend")
	} else {
		log.Info("successfully released lock on host-local backend!")
	}

	// Delete the host-local IPAM data directory.
	log.Info("removing host-local IPAM data directory")
	if err = os.RemoveAll(ipAllocPath); err != nil && !os.IsNotExist(err) {
		log.WithError(err).Error("failed to remove host-local IPAM data dir directory")
	}
	log.Info("successfully removed host-local IPAM data directory!")

	// HACK: Always re-enable datastoreReady.
	// TODO: Should check DaemonSet rolling update status to see if this is the last node to update and
	// only re-enable datastoreReady if the number of UpdatedNumberScheduled is MaxUnavailable less than the DesiredNumberScheduled.
	// calicoDS, err := k8sClient.AppsV1().DaemonSets("kube-system").Get("calico-node", metav1.GetOptions{})
	// if (calicoDS.Status.UpdatedNumberScheduled == calicoDS.Status.DesiredNumberScheduled-calicoDS.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable.IntVal)
	log.Info("setting Calico datastore readiness to true...")

	// Implemented retry on conflict, because we get stuck if the upgrade-ipam
	// container fails here and the datastore doesn't get ready again
	for i := uint(0); i < 5; i++ {
		clusterInfo, err := c.ClusterInformation().Get(ctxt, "default", options.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to fetch cluster information: %s", err)
		}
		t := true
		clusterInfo.Spec.DatastoreReady = &t
		if clusterInfo, err = c.ClusterInformation().Update(ctxt, clusterInfo, options.SetOptions{}); err != nil {
			if _, ok := err.(errors.ErrorResourceUpdateConflict); ok {
				log.Info("Encountered update conflict, retrying...")
				time.Sleep(1 * time.Second)
				continue
			}
			return fmt.Errorf("failed to re-enable cluster by updating cluster informations: %s", err)
		}
		break
	}

	log.Info("successfully set Calico datastore readiness to true!")

	return nil
}

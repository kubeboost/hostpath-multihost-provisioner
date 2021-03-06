/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"time"

	"github.com/golang/glog"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/sig-storage-lib-external-provisioner/v6/controller"
	"syscall"
)

const (
	// The name used to identify this provisioner.
	// NOTE: It is expected to have only one replica of the hostpath-multihost-provisioner so
	// the same name is used for all the provisioner.
	provisionerName = "kubeboost.github.com/hostpath-multihost-provisioner"

	// The annotation key used to identify the resources owned by this provisioner.
	provisionerIdentityLabel = provisionerName + "-identity"

	// The service identifying manager pods. It shall be a headless service, to be able to retrieve
	// all the pods managed by the SRV record.
	storageManagerServiceName = "hostpath-multihost-manager"

	// The port where manager pods are listening.
	storageManagerServicePort = "8080"

	// The directory in the manager pods where volumes are created. It is not configurable anymore
	// as it does not provides any benefit for the user to change the location inside the pod.
	pvDir = "/var/kubernetes"

	// Max number of times a request is retried.
	maxRetries = 5
)

type hostPathProvisioner struct {
	// Identity of this hostPathProvisioner, set to node's name. Used to identify
	// "this" provisioner's PVs.
	identity string
}

var _ controller.Provisioner = &hostPathProvisioner{}

// Provision sends a request to every manager to create a storage asset in every node and returns a PV object representing it.
func (p *hostPathProvisioner) Provision(_ context.Context, options controller.ProvisionOptions) (*v1.PersistentVolume, controller.ProvisioningState, error) {
	// Compute path in the manager pods where persistent volumes are going to be created.
	path := path.Join(pvDir, options.PVC.Namespace+"-"+options.PVC.Name+"-"+options.PVName)
	glog.Infof("Creating backing directory: %v", path)

	// Send a creation request of the computed path to every manager pod.
	// Manager runs as DaemonSet. So this path is going to be created on every node.
	if err := sendRequestToManager(path, createDir); err != nil {
		return nil, controller.ProvisioningFinished, err
	}

	// Get the storagge class reclaim policy.
	reclaimPolicy := *options.StorageClass.ReclaimPolicy

	// Create the new persistent volume with the computed path and policy.
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: options.PVName,
			Annotations: map[string]string{
				provisionerIdentityLabel: p.identity,
			},
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: reclaimPolicy,
			AccessModes:                   options.PVC.Spec.AccessModes,
			Capacity: v1.ResourceList{
				v1.ResourceName(v1.ResourceStorage): options.PVC.Spec.Resources.Requests[v1.ResourceName(v1.ResourceStorage)],
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: path,
				},
			},
		},
	}

	// Return the created persistent volume successfully.
	return pv, controller.ProvisioningFinished, nil
}

// This struct represents and http status error. Used to return error when status is not 200 OK.
type httpStatusError struct {
	status int
}

func (e httpStatusError) Error() string {
	return fmt.Sprintf("HTTP Status Error with status code: %v", e.status)
}

// A function which performs a request agains the managers rest API.
// Providing the ip of the manager, and the filesystem path of the object to manage.
// It returns an error because the function can fail if the reques fails.
type managerRequestFunction func(ip string, path string) error

// It sends a request to every manager monitored by the manager service.
// The requests are sent in parallel to every manager pod.
// It returns an error if any of the request fails.
func sendRequestToManager(path string, requestFunc managerRequestFunction) error {
	// Resolv every DNS behind headless service for manager.
	glog.Infof("Looking for service %q.", storageManagerServiceName)
	ips, err := net.LookupHost(storageManagerServiceName)
	if err != nil {
		glog.Errorf("Error looking for service: %q", err.Error())
		return err
	}

	// Perform a request in parallel to every manager monitored by the manager service.
	glog.Infof("Start sending requests.")
	results := make(chan error)
	for _, ip := range ips {
		go func(ip string) {
			var result error
			// Try up to max retries for the request to succeed.
			for retries := 0; retries < maxRetries; retries++ {
				result = requestFunc(ip, path)
				if result == nil {
					break
				}

				glog.Infof("Failed to send request to %q. Retry %v of %v.", ip, retries, maxRetries)
				time.Sleep(1)
			}
			results <- result
		}(ip)
	}

	// Wait for every request to finish and return error if any fail.
	for range ips {
		err := <-results
		if err != nil {
			return err
		}
	}

	return nil
}

// Send a POST request to create a directory at the given filesystem path to the provided ip address.
// It returns an error if there is any problem sending the request.
func createDir(ip string, path string) error {
	targetUrl := fmt.Sprintf("http://%v:%v/directories", ip, storageManagerServicePort)

	// Send the creation request to manager.
	glog.Infof("Sending POST request to %q, with path %q.", targetUrl, path)
	resp, err := http.PostForm(targetUrl, url.Values{"path": {path}})
	if err != nil {
		return err
	}

	// Ensure to close the response body at the end.
	defer resp.Body.Close()

	// If the status code is not successfull return an httpStatusError.
	if resp.StatusCode != http.StatusOK {
		return httpStatusError{resp.StatusCode}
	}

	return nil
}

// Send a DELETE request to remove a directory at the given filesystem path to the provided ip address.
// It returns an error if there is any problem sending the request.
func deleteDir(ip string, path string) error {
	targetUrl := fmt.Sprintf("http://%v:%v/directories?path=%v", ip, storageManagerServicePort, path)

	// Create DELETE request.
	glog.Infof("Sending DELETE request to %q, with path %q.", targetUrl, path)
	req, err := http.NewRequest(http.MethodDelete, targetUrl, nil)
	if err != nil {
		return err
	}

	// Send DELETE request to manager.
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	// Ensure to close the response body at the end.
	defer resp.Body.Close()

	// If the status code is not successfull return an httpStatusError.
	if resp.StatusCode != http.StatusOK {
		return httpStatusError{resp.StatusCode}
	}

	return nil
}

// Delete removes the storage asset that was created by Provision represented
// by the given PV.
func (p *hostPathProvisioner) Delete(ctx context.Context, volume *v1.PersistentVolume) error {
	// Check that the deleted volume is managed by this provisioner. Otherwise, ignore it.
	ann, ok := volume.Annotations[provisionerIdentityLabel]
	if !ok {
		return errors.New("identity annotation not found on PV")
	}
	if ann != p.identity {
		return &controller.IgnoredError{Reason: "identity annotation on PV does not match ours"}
	}

	// Send a DELETE request to managers to remove the volume in every manager pod.
	// This will delete the contents of this volume on every node.
	path := volume.Spec.PersistentVolumeSource.HostPath.Path
	glog.Info("Removing backing directory: %v", path)

	if err := sendRequestToManager(path, deleteDir); err != nil {
		return err
	}

	return nil
}

func main() {
	syscall.Umask(0)

	flag.Parse()
	flag.Set("logtostderr", "true")

	// Create an InClusterConfig and use it to create a client for the controller
	// to use to communicate with Kubernetes
	config, err := rest.InClusterConfig()
	if err != nil {
		glog.Fatalf("Failed to create config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		glog.Fatalf("Failed to create client: %v", err)
	}

	// The controller needs to know what the server version is because out-of-tree
	// provisioners aren't officially supported until 1.5
	serverVersion, err := clientset.Discovery().ServerVersion()
	if err != nil {
		glog.Fatalf("Error getting server version: %v", err)
	}

	// Allow to enable or disable leader election using environment variable ENABLE_LEADER_ELECTION.
	leaderElection := true
	leaderElectionEnv := os.Getenv("ENABLE_LEADER_ELECTION")
	if leaderElectionEnv != "" {
		leaderElection, err = strconv.ParseBool(leaderElectionEnv)
		if err != nil {
			glog.Fatalf("Unable to parse ENABLE_LEADER_ELECTION env var: %v", err)
		}
	}

	// Create the provisioner: it implements the Provisioner interface expected by
	// the controller
	hostPathProvisioner := &hostPathProvisioner{
		provisionerName,
	}

	// Start the provision controller which will dynamically provision hostPath
	// PVs
	pc := controller.NewProvisionController(clientset,
		provisionerName,
		hostPathProvisioner,
		serverVersion.GitVersion,
		controller.LeaderElection(leaderElection),
	)

	// Start executing the provisioner.
	pc.Run(context.Background())
}

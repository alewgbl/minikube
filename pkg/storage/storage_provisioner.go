/*
Copyright 2016 The Kubernetes Authors All rights reserved.

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

package storage

import (
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/sig-storage-lib-external-provisioner/v5/controller"
)

const provisionerName = "k8s.io/minikube-hostpath"
const provisionerAnnotation = "pv.kubernetes.io/provisioned-by"

type hostPathProvisioner struct {
	// The directory to create PV-backing directories in
	pvDir string

	// Identity of this hostPathProvisioner, generated. Used to identify "this"
	// provisioner's PVs.
	identity types.UID
}

// NewHostPathProvisioner creates a new Provisioner using host paths
func NewHostPathProvisioner(pvDir string) controller.Provisioner {
	return &hostPathProvisioner{
		pvDir:    pvDir,
		identity: uuid.NewUUID(),
	}
}

var _ controller.Provisioner = new(hostPathProvisioner)

// Provision creates a storage asset and returns a PV object representing it.
func (p *hostPathProvisioner) Provision(options controller.ProvisionOptions) (*core.PersistentVolume, error) {
	pvPath := path.Join(p.pvDir, options.PVC.Namespace, options.PVC.Name)

	// SANITY CHECK: If the pvPath already exists then we do not want to overwrite it
	pvPathFileInfo, err := os.Stat(pvPath)
	if err != nil {
		// If the directory doesn't exist, that's good. Otherwise, log an error
		if !strings.HasSuffix(err.Error(), "no such file or directory") {
			klog.Errorf("os.Stat(%s) error: %v", pvPath, err)
		}
		// Continue on since that is the previous behaviour
	} else {
		if pvPathFileInfo.IsDir() {
			// The PV directory already exists so we do not want to go any further
			return nil, fmt.Errorf("PV directory %s already exists and we will not overwrite it", pvPath)
		}
	}

	klog.Infof("Provisioning volume %v to %s", options, pvPath)
	if err := os.MkdirAll(pvPath, 0777); err != nil {
		return nil, err
	}

	// Explicitly chmod created dir, so we know mode is set to 0777 regardless of umask
	if err := os.Chmod(pvPath, 0777); err != nil {
		return nil, err
	}

	pv := &core.PersistentVolume{
		ObjectMeta: meta.ObjectMeta{
			Name: options.PVName,
			Annotations: map[string]string{
				"hostPathProvisionerIdentity": string(p.identity),
			},
		},
		Spec: core.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: *options.StorageClass.ReclaimPolicy,
			AccessModes:                   options.PVC.Spec.AccessModes,
			Capacity: core.ResourceList{
				core.ResourceStorage: options.PVC.Spec.Resources.Requests[core.ResourceStorage],
			},
			PersistentVolumeSource: core.PersistentVolumeSource{
				HostPath: &core.HostPathVolumeSource{
					Path: pvPath,
				},
			},
		},
	}

	return pv, nil
}

// Delete removes the storage asset that was created by Provision represented
// by the given PV.
func (p *hostPathProvisioner) Delete(volume *core.PersistentVolume) error {
	klog.Infof("Deleting volume %v", volume)

	// Look up the hostPathProvisionerIdentity
	ann, ok := volume.Annotations["hostPathProvisionerIdentity"]
	if !ok {
		return errors.New("identity annotation not found on PV")
	}
	// If our UUID doesn't match the hostPathProvisionerIdentity, then "this" instance
	// of the provisioner didn't provision the PV. However, there's a good chance that
	// a Minikube hostpath provisioner was used to provision this volume, so let's check
	// for that.
	if ann != string(p.identity) {
		pvProvisioner, ok := volume.Annotations[provisionerAnnotation]
		if !ok {
			return fmt.Errorf("%s annotation not found on PV", provisionerAnnotation)
		}
		// Check if the volume was provisioned on a node of the same name as the one we're on
		if pvProvisioner != provisionerName {
			// The volume wasn't provisioned by this kind of provisioner; do nothing further
			return &controller.IgnoredError{
				Reason: fmt.Sprintf("volume was provisioned by a %s provisioner but we are a %s provisioner; will not delete the volume", pvProvisioner, provisionerName),
			}
		}
		klog.Infof("identity annotation on PV (%s) did not match ours (%s), but the volume was provisioned by a %s provisioner and that is okay",
			ann,
			p.identity,
			pvProvisioner)
	}

	if err := os.RemoveAll(volume.Spec.PersistentVolumeSource.HostPath.Path); err != nil {
		return errors.Wrap(err, "removing hostpath PV")
	}

	return nil
}

// StartStorageProvisioner will start storage provisioner server
func StartStorageProvisioner(pvDir string) error {
	klog.Infof("Initializing the minikube storage provisioner...")
	config, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Failed to create client: %v", err)
	}

	// The controller needs to know what the server version is because out-of-tree
	// provisioners aren't officially supported until 1.5
	serverVersion, err := clientset.Discovery().ServerVersion()
	if err != nil {
		return fmt.Errorf("error getting server version: %v", err)
	}

	// Create the provisioner: it implements the Provisioner interface expected by
	// the controller
	hostPathProvisioner := NewHostPathProvisioner(pvDir)

	// Start the provision controller which will dynamically provision hostPath
	// PVs
	pc := controller.NewProvisionController(clientset, provisionerName, hostPathProvisioner, serverVersion.GitVersion)

	klog.Info("Storage provisioner initialized, now starting service!")
	pc.Run(wait.NeverStop)
	return nil
}

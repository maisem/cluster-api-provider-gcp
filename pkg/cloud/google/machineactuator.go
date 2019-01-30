/*
Copyright 2018 The Kubernetes Authors.

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

package google

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/golang/glog"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"

	"github.com/ghodss/yaml"
	gcfg "gopkg.in/gcfg.v1"
	gceconfigv1 "sigs.k8s.io/cluster-api-provider-gcp/pkg/apis/gceproviderconfig/v1alpha1"
	"sigs.k8s.io/cluster-api-provider-gcp/pkg/cloud/google/clients"
	"sigs.k8s.io/cluster-api-provider-gcp/pkg/cloud/google/machinesetup"
	clusterv1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
	"sigs.k8s.io/cluster-api/pkg/cert"
	apierrors "sigs.k8s.io/cluster-api/pkg/errors"
	"sigs.k8s.io/cluster-api/pkg/kubeadm"
	"sigs.k8s.io/cluster-api/pkg/util"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ProjectAnnotationKey = "gcp-project"
	ZoneAnnotationKey    = "gcp-zone"
	NameAnnotationKey    = "gcp-name"

	BootstrapLabelKey = "bootstrap"

	// This file is a yaml that will be used to create the machine-setup configmap on the machine controller.
	// It contains the supported machine configurations along with the startup scripts and OS image paths that correspond to each supported configuration.
	MachineSetupConfigsFilename = "machine_setup_configs.yaml"
	ProviderName                = "google"
)

const (
	createEventAction = "Create"
	deleteEventAction = "Delete"
	noEventAction     = ""
)

var MachineActuator *GCEClient

type SshCreds struct {
	user           string
	privateKeyPath string
}

type GCEClientKubeadm interface {
	TokenCreate(params kubeadm.TokenCreateParams) (string, error)
}

type GCEClientMachineSetupConfigGetter interface {
	GetMachineSetupConfig() (machinesetup.MachineSetupConfig, error)
}

type GCEClient struct {
	certificateAuthority     *cert.CertificateAuthority
	computeService           GCEClientComputeService
	kubeadm                  GCEClientKubeadm
	serviceAccountService    *ServiceAccountService
	sshCreds                 SshCreds
	client                   client.Client
	machineSetupConfigGetter GCEClientMachineSetupConfigGetter
	eventRecorder            record.EventRecorder
	scheme                   *runtime.Scheme
}

type MachineActuatorParams struct {
	CertificateAuthority     *cert.CertificateAuthority
	ComputeService           GCEClientComputeService
	Kubeadm                  GCEClientKubeadm
	Client                   client.Client
	MachineSetupConfigGetter GCEClientMachineSetupConfigGetter
	EventRecorder            record.EventRecorder
	Scheme                   *runtime.Scheme
	CloudConfigPath          string
}

func NewMachineActuator(params MachineActuatorParams) (*GCEClient, error) {
	computeService, err := getOrNewComputeServiceForMachine(params.ComputeService, params.CloudConfigPath)
	if err != nil {
		return nil, err
	}

	serviceAccountService := NewServiceAccountService()

	// Only applicable if it's running inside machine controller pod.
	var privateKeyPath, user string
	if _, err := os.Stat("/etc/sshkeys/private"); err == nil {
		privateKeyPath = "/etc/sshkeys/private"

		b, err := ioutil.ReadFile("/etc/sshkeys/user")
		if err != nil {
			return nil, err
		}
		user = string(b)
	}

	return &GCEClient{
		certificateAuthority:  params.CertificateAuthority,
		computeService:        computeService,
		kubeadm:               getOrNewKubeadm(params),
		serviceAccountService: serviceAccountService,
		sshCreds: SshCreds{
			privateKeyPath: privateKeyPath,
			user:           user,
		},
		client: params.Client,
		machineSetupConfigGetter: params.MachineSetupConfigGetter,
		eventRecorder:            params.EventRecorder,
		scheme:                   params.Scheme,
	}, nil
}

// TODO move the following four functions to a separate file?
func clusterProviderFromProviderSpec(providerSpec clusterv1.ProviderSpec) (*gceconfigv1.GCEClusterProviderSpec, error) {
	var config gceconfigv1.GCEClusterProviderSpec
	if err := yaml.Unmarshal(providerSpec.Value.Raw, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

func machineProviderFromProviderSpec(providerSpec clusterv1.ProviderSpec) (*gceconfigv1.GCEMachineProviderSpec, error) {
	var config gceconfigv1.GCEMachineProviderSpec
	if err := yaml.Unmarshal(providerSpec.Value.Raw, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

// TODO these two funcs shouldn't be exported, but need to be for testing...
func ProviderSpecFromCluster(in *gceconfigv1.GCEClusterProviderSpec) (*clusterv1.ProviderSpec, error) {
	mpc, err := yaml.Marshal(in)
	if err != nil {
		return nil, err
	}
	return &clusterv1.ProviderSpec{
		Value: &runtime.RawExtension{Raw: mpc},
	}, nil
}

func ProviderSpecFromMachine(in *gceconfigv1.GCEMachineProviderSpec) (*clusterv1.ProviderSpec, error) {
	mpc, err := yaml.Marshal(in)
	if err != nil {
		return nil, err
	}
	return &clusterv1.ProviderSpec{
		Value: &runtime.RawExtension{Raw: mpc},
	}, nil
}

func (gce *GCEClient) CreateMachineController(cluster *clusterv1.Cluster, initialMachines []*clusterv1.Machine, clientSet kubernetes.Clientset) error {
	if gce.machineSetupConfigGetter == nil {
		return errors.New("a valid machineSetupConfigGetter is required")
	}
	if err := gce.serviceAccountService.CreateMachineControllerServiceAccount(cluster); err != nil {
		return err
	}

	// Setup SSH access to master VM
	if err := gce.setupSSHAccess(cluster, util.GetMaster(initialMachines)); err != nil {
		return err
	}

	if err := CreateExtApiServerRoleBinding(); err != nil {
		return err
	}

	machineSetupConfig, err := gce.machineSetupConfigGetter.GetMachineSetupConfig()
	if err != nil {
		return err
	}
	yaml, err := machineSetupConfig.GetYaml()
	if err != nil {
		return err
	}
	configMap := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-setup"},
		Data: map[string]string{
			MachineSetupConfigsFilename: yaml,
		},
	}
	configMaps := clientSet.CoreV1().ConfigMaps(corev1.NamespaceDefault)
	if _, err := configMaps.Create(&configMap); err != nil {
		return err
	}

	if err := CreateApiServerAndController(); err != nil {
		return err
	}
	return nil
}

func (gce *GCEClient) ProvisionClusterDependencies(cluster *clusterv1.Cluster) error {
	err := gce.serviceAccountService.CreateWorkerNodeServiceAccount(cluster)
	if err != nil {
		return err
	}

	return gce.serviceAccountService.CreateMasterNodeServiceAccount(cluster)
}

func (gce *GCEClient) Create(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine) error {
	if machine.Annotations != nil && machine.Annotations["mig-based"] == "true" {
		return gce.updateAnnotations(ctx, cluster, machine)
	}
	machineConfig, err := machineProviderFromProviderSpec(machine.Spec.ProviderSpec)
	if err != nil {
		return gce.handleMachineError(ctx, machine, apierrors.InvalidMachineConfiguration(
			"Cannot unmarshal machine's providerSpec field: %v", err), createEventAction)
	}

	if err := gce.validateMachine(machine, machineConfig); err != nil {
		return gce.handleMachineError(ctx, machine, err, createEventAction)
	}
	instance, err := gce.instanceIfExists(ctx, cluster, machine)
	if err != nil {
		return err
	}

	if instance != nil {
		glog.Infof("Skipped creating a VM that already exists.\n")
		return nil
	}

	var op *compute.Operation
	if machineConfig.InstanceTemplate != "" {
		op, err = gce.createFromInstanceTemplate(ctx, cluster, machine, machineConfig)
	} else {
		op, err = gce.create(ctx, cluster, machine, machineConfig)
	}
	if err == nil {
		err = gce.computeService.WaitForOperation(ctx, machineConfig.Project, op)
	}
	if err != nil {
		return gce.handleMachineError(ctx, machine, apierrors.CreateMachine(
			"error creating GCE instance: %v", err), createEventAction)
	}

	gce.eventRecorder.Eventf(machine, corev1.EventTypeNormal, "Created", "Created Machine %v", machine.Name)
	// If we have a v1Alpha1Client, then annotate the machine so that we
	// remember exactly what VM we created for it.
	if gce.client != nil {
		return gce.updateAnnotations(ctx, cluster, machine)
	}
	return nil
}

func (gce *GCEClient) createFromInstanceTemplate(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, machineConfig *gceconfigv1.GCEMachineProviderSpec) (*compute.Operation, error) {
	return gce.computeService.InstancesInsertFromTemplate(ctx, machineConfig.Project, machineConfig.Zone, machine.Name, machineConfig.InstanceTemplate)
}

func (gce *GCEClient) create(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, machineConfig *gceconfigv1.GCEMachineProviderSpec) (*compute.Operation, error) {
	if gce.machineSetupConfigGetter == nil {
		return nil, errors.New("a valid machineSetupConfigGetter is required")
	}
	clusterConfig, err := clusterProviderFromProviderSpec(cluster.Spec.ProviderSpec)
	if err != nil {
		return nil, gce.handleMachineError(ctx, machine, apierrors.InvalidMachineConfiguration(
			"Cannot unmarshal cluster's providerSpec field: %v", err), createEventAction)
	}
	configParams := &machinesetup.ConfigParams{
		OS:       machineConfig.OS,
		Roles:    machineConfig.Roles,
		Versions: machine.Spec.Versions,
	}

	machineSetupConfigs, err := gce.machineSetupConfigGetter.GetMachineSetupConfig()
	if err != nil {
		return nil, err
	}
	image, err := machineSetupConfigs.GetImage(configParams)
	if err != nil {
		return nil, err
	}
	imagePath := gce.getImagePath(ctx, image)
	metadata, err := gce.getMetadata(ctx, cluster, clusterConfig, machine, machineConfig, configParams)
	if err != nil {
		return nil, err
	}

	labels := map[string]string{}
	if gce.client == nil {
		labels[BootstrapLabelKey] = "true"
	}

	return gce.computeService.InstancesInsert(ctx, machineConfig.Project, machineConfig.Zone, &compute.Instance{
		Name:         machine.Name,
		MachineType:  fmt.Sprintf("zones/%s/machineTypes/%s", machineConfig.Zone, machineConfig.MachineType),
		CanIpForward: true,
		NetworkInterfaces: []*compute.NetworkInterface{
			{
				Network: "global/networks/default",
				AccessConfigs: []*compute.AccessConfig{
					{
						Type: "ONE_TO_ONE_NAT",
						Name: "External NAT",
					},
				},
			},
		},
		Disks:    newDisks(machineConfig, machineConfig.Zone, imagePath, int64(30)),
		Metadata: metadata,
		Tags: &compute.Tags{
			Items: []string{
				"https-server",
				fmt.Sprintf("%s-worker", cluster.Name)},
		},
		Labels: labels,
		ServiceAccounts: []*compute.ServiceAccount{
			{
				Email: gce.serviceAccountService.GetDefaultServiceAccountForMachine(cluster, machine),
				Scopes: []string{
					compute.CloudPlatformScope,
				},
			},
		},
	})
}

func (gce *GCEClient) Delete(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine) error {
	if machine.Annotations != nil && machine.Annotations["mig-based"] == "true" {
		return nil
	}
	instance, err := gce.instanceIfExists(ctx, cluster, machine)
	if err != nil {
		return err
	}

	if instance == nil {
		glog.Infof("Skipped deleting a VM that is already deleted.\n")
		return nil
	}

	machineConfig, err := machineProviderFromProviderSpec(machine.Spec.ProviderSpec)
	if err != nil {
		return gce.handleMachineError(ctx, machine,
			apierrors.InvalidMachineConfiguration("Cannot unmarshal machine's providerSpec field: %v", err), deleteEventAction)
	}
	if err := gce.validateMachine(machine, machineConfig); err != nil {
		return gce.handleMachineError(ctx, machine, err, deleteEventAction)
	}

	var project, zone, name string

	if machine.ObjectMeta.Annotations != nil {
		project = machine.ObjectMeta.Annotations[ProjectAnnotationKey]
		zone = machine.ObjectMeta.Annotations[ZoneAnnotationKey]
		name = machine.ObjectMeta.Annotations[NameAnnotationKey]
	}

	// If the annotations are missing, fall back on providerSpec
	if project == "" || zone == "" || name == "" {
		project = machineConfig.Project
		zone = machineConfig.Zone
		name = machine.ObjectMeta.Name
	}

	op, err := gce.computeService.InstancesDelete(ctx, project, zone, name)
	if err == nil {
		err = gce.computeService.WaitForOperation(ctx, project, op)
	}
	if err != nil {
		return gce.handleMachineError(ctx, machine, apierrors.DeleteMachine(
			"error deleting GCE instance: %v", err), deleteEventAction)
	}

	gce.eventRecorder.Eventf(machine, corev1.EventTypeNormal, "Deleted", "Deleted Machine %v", name)

	return err
}

func (gce *GCEClient) PostDelete(cluster *clusterv1.Cluster) error {
	if err := gce.serviceAccountService.DeleteMasterNodeServiceAccount(cluster); err != nil {
		return fmt.Errorf("error deleting master node service account: %v", err)
	}
	if err := gce.serviceAccountService.DeleteWorkerNodeServiceAccount(cluster); err != nil {
		return fmt.Errorf("error deleting worker node service account: %v", err)
	}
	if err := gce.serviceAccountService.DeleteIngressControllerServiceAccount(cluster); err != nil {
		return fmt.Errorf("error deleting ingress controller service account: %v", err)
	}
	if err := gce.serviceAccountService.DeleteMachineControllerServiceAccount(cluster); err != nil {
		return fmt.Errorf("error deleting machine controller service account: %v", err)
	}
	return nil
}

func (gce *GCEClient) Update(ctx context.Context, cluster *clusterv1.Cluster, goalMachine *clusterv1.Machine) error {
	// Before updating, do some basic validation of the object first.
	goalConfig, err := machineProviderFromProviderSpec(goalMachine.Spec.ProviderSpec)
	if err != nil {
		return gce.handleMachineError(ctx, goalMachine,
			apierrors.InvalidMachineConfiguration("Cannot unmarshal machine's providerSpec field: %v", err), noEventAction)
	}
	if verr := gce.validateMachine(goalMachine, goalConfig); verr != nil {
		return gce.handleMachineError(ctx, goalMachine, verr, noEventAction)
	}

	status, err := gce.instanceStatus(goalMachine)
	if err != nil {
		return err
	}

	currentMachine := (*clusterv1.Machine)(status)
	if currentMachine == nil {
		_, err := gce.instanceIfExists(ctx, cluster, goalMachine)
		if err != nil {
			return err
		}
		glog.Infof("Populating current state for bootstrap machine %v", goalMachine.ObjectMeta.Name)
		return gce.updateAnnotations(ctx, cluster, goalMachine)
	}

	currentConfig, err := machineProviderFromProviderSpec(currentMachine.Spec.ProviderSpec)
	if err != nil {
		return gce.handleMachineError(ctx, currentMachine, apierrors.InvalidMachineConfiguration(
			"Cannot unmarshal machine's providerSpec field: %v", err), noEventAction)
	}

	if !gce.requiresUpdate(currentMachine, goalMachine) {
		return nil
	}

	if isMaster(currentConfig.Roles) {
		glog.Infof("Doing an in-place upgrade for master.\n")
		// TODO: should we support custom CAs here?
		err = gce.updateMasterInplace(cluster, currentMachine, goalMachine)
		if err != nil {
			glog.Errorf("master inplace update failed: %v", err)
		}
	} else {
		glog.Infof("re-creating machine %s for update.", currentMachine.ObjectMeta.Name)
		err = gce.Delete(ctx, cluster, currentMachine)
		if err != nil {
			glog.Errorf("delete machine %s for update failed: %v", currentMachine.ObjectMeta.Name, err)
		} else {
			err = gce.Create(ctx, cluster, goalMachine)
			if err != nil {
				glog.Errorf("create machine %s for update failed: %v", goalMachine.ObjectMeta.Name, err)
			}
		}
	}
	if err != nil {
		return err
	}
	return gce.updateInstanceStatus(ctx, goalMachine)
}

func (gce *GCEClient) Exists(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine) (bool, error) {
	i, err := gce.instanceIfExists(ctx, cluster, machine)
	if err != nil {
		return false, err
	}
	return (i != nil), err
}

func (gce *GCEClient) GetIP(cluster *clusterv1.Cluster, machine *clusterv1.Machine) (string, error) {
	machineConfig, err := machineProviderFromProviderSpec(machine.Spec.ProviderSpec)
	if err != nil {
		return "", err
	}
	instance, err := gce.computeService.InstancesGet(context.TODO(), machineConfig.Project, machineConfig.Zone, machine.ObjectMeta.Name)
	if err != nil {
		return "", err
	}

	var publicIP string

	for _, networkInterface := range instance.NetworkInterfaces {
		if networkInterface.Name == "nic0" {
			for _, accessConfigs := range networkInterface.AccessConfigs {
				publicIP = accessConfigs.NatIP
			}
		}
	}
	return publicIP, nil
}

func (gce *GCEClient) GetKubeConfig(cluster *clusterv1.Cluster, master *clusterv1.Machine) (string, error) {
	machineConfig, err := machineProviderFromProviderSpec(master.Spec.ProviderSpec)
	if err != nil {
		return "", err
	}

	command := "sudo cat /etc/kubernetes/admin.conf"
	result := strings.TrimSpace(util.ExecCommand(
		"gcloud", "compute", "ssh", "--project", machineConfig.Project,
		"--zone", machineConfig.Zone, master.ObjectMeta.Name, "--command", command, "--", "-q"))
	return result, nil
}

func isMaster(roles []gceconfigv1.MachineRole) bool {
	for _, r := range roles {
		if r == gceconfigv1.MasterRole {
			return true
		}
	}
	return false
}

func (gce *GCEClient) updateAnnotations(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine) error {
	machineConfig, err := machineProviderFromProviderSpec(machine.Spec.ProviderSpec)
	name := machine.ObjectMeta.Name
	zone := machineConfig.Zone
	if err != nil {
		return gce.handleMachineError(ctx, machine,
			apierrors.InvalidMachineConfiguration("Cannot unmarshal machine's providerSpec field: %v", err), noEventAction)
	}
	if machine.ObjectMeta.Annotations == nil {
		machine.ObjectMeta.Annotations = make(map[string]string)
	}
	machine.ObjectMeta.Annotations[ProjectAnnotationKey] = machineConfig.Project
	machine.ObjectMeta.Annotations[ZoneAnnotationKey] = zone
	machine.ObjectMeta.Annotations[NameAnnotationKey] = name
	if err := gce.client.Update(ctx, machine); err != nil {
		return err
	}
	id := fmt.Sprintf("gce://%s/%s/%s", machineConfig.Project, zone, name)
	machine.Status.ProviderID = &id
	if err := gce.client.Status().Update(ctx, machine); err != nil {
		return err
	}
	return gce.updateInstanceStatus(ctx, machine)
}

// The two machines differ in a way that requires an update
func (gce *GCEClient) requiresUpdate(a *clusterv1.Machine, b *clusterv1.Machine) bool {
	// Do not want status changes. Do want changes that impact machine provisioning
	return !reflect.DeepEqual(a.Spec.ObjectMeta, b.Spec.ObjectMeta) ||
		!reflect.DeepEqual(a.Spec.ProviderSpec, b.Spec.ProviderSpec) ||
		!reflect.DeepEqual(a.Spec.Versions, b.Spec.Versions) ||
		a.ObjectMeta.Name != b.ObjectMeta.Name
}

// Gets the instance represented by the given machine
func (gce *GCEClient) instanceIfExists(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine) (*compute.Instance, error) {
	identifyingMachine := machine

	// Try to use the last saved status locating the machine
	// in case instance details like the proj or zone has changed
	status, err := gce.instanceStatus(machine)
	if err != nil {
		return nil, err
	}

	if status != nil {
		identifyingMachine = (*clusterv1.Machine)(status)
	}

	// Get the VM via specified location and name
	machineConfig, err := machineProviderFromProviderSpec(identifyingMachine.Spec.ProviderSpec)
	if err != nil {
		return nil, err
	}

	instance, err := gce.computeService.InstancesGet(ctx, machineConfig.Project, machineConfig.Zone, identifyingMachine.ObjectMeta.Name)
	if err != nil {
		if gerr, ok := err.(*googleapi.Error); ok && gerr.Code == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}

	return instance, nil
}

func (gce *GCEClient) updateMasterInplace(cluster *clusterv1.Cluster, oldMachine *clusterv1.Machine, newMachine *clusterv1.Machine) error {
	if oldMachine.Spec.Versions.ControlPlane != newMachine.Spec.Versions.ControlPlane {
		cmd := fmt.Sprintf(
			"curl -fsSL https://dl.k8s.io/release/v%s/bin/linux/amd64/kubeadm | sudo tee /usr/bin/kubeadm > /dev/null; "+
				"sudo chmod a+rx /usr/bin/kubeadm", newMachine.Spec.Versions.ControlPlane)
		_, err := gce.remoteSshCommand(cluster, newMachine, cmd)
		if err != nil {
			glog.Infof("remotesshcomand error: %v", err)
			return err
		}

		// TODO: We might want to upgrade kubeadm if the target control plane version is newer.
		// Upgrade control plan.
		cmd = fmt.Sprintf("sudo kubeadm upgrade apply %s -y", "v"+newMachine.Spec.Versions.ControlPlane)
		_, err = gce.remoteSshCommand(cluster, newMachine, cmd)
		if err != nil {
			glog.Infof("remotesshcomand error: %v", err)
			return err
		}
	}

	// Upgrade kubelet.
	if oldMachine.Spec.Versions.Kubelet != newMachine.Spec.Versions.Kubelet {
		cmd := fmt.Sprintf("sudo kubectl drain %s --kubeconfig /etc/kubernetes/admin.conf --ignore-daemonsets", newMachine.Name)
		// The errors are intentionally ignored as master has static pods.
		gce.remoteSshCommand(cluster, newMachine, cmd)
		// Upgrade kubelet to desired version.
		cmd = fmt.Sprintf("sudo apt-get install kubelet=%s", newMachine.Spec.Versions.Kubelet+"-00")
		_, err := gce.remoteSshCommand(cluster, newMachine, cmd)
		if err != nil {
			glog.Infof("remotesshcomand error: %v", err)
			return err
		}
		cmd = fmt.Sprintf("sudo kubectl uncordon %s --kubeconfig /etc/kubernetes/admin.conf", newMachine.Name)
		_, err = gce.remoteSshCommand(cluster, newMachine, cmd)
		if err != nil {
			glog.Infof("remotesshcomand error: %v", err)
			return err
		}
	}

	return nil
}

func (gce *GCEClient) validateMachine(machine *clusterv1.Machine, config *gceconfigv1.GCEMachineProviderSpec) *apierrors.MachineError {
	if machine.Spec.Versions.Kubelet == "" {
		return apierrors.InvalidMachineConfiguration("spec.versions.kubelet can't be empty")
	}
	if config.MachineType == "" && config.InstanceTemplate == "" {
		return apierrors.InvalidMachineConfiguration("spec.machineType or spec.instanceTemplate can't be empty")
	}
	if config.InstanceTemplate != "" {
		if config.OS != "" {
			glog.Warning("machine.Spec.InstanceTemplate is set; machine.Spec.OS ignored")
		}
		if len(config.Disks) > 0 {
			glog.Warning("machine.Spec.InstanceTemplate is set; machine.Spec.Disks ignored")
		}
	}
	return nil
}

// If the GCEClient has a client for updating Machine objects, this will set
// the appropriate reason/message on the Machine.Status. If not, such as during
// cluster installation, it will operate as a no-op. It also returns the
// original error for convenience, so callers can do "return handleMachineError(...)".
func (gce *GCEClient) handleMachineError(ctx context.Context, machine *clusterv1.Machine, err *apierrors.MachineError, eventAction string) error {
	glog.Errorf("Machine error: %v", err.Message)
	if gce.client != nil {
		reason := err.Reason
		message := err.Message
		machine.Status.ErrorReason = &reason
		machine.Status.ErrorMessage = &message
		if err := gce.client.Status().Update(ctx, machine); err != nil {
			return err
		}
	}

	if eventAction != noEventAction {
		gce.eventRecorder.Eventf(machine, corev1.EventTypeWarning, "Failed"+eventAction, "%v", err.Reason)
	}
	return err
}

func (gce *GCEClient) getImagePath(ctx context.Context, img string) (imagePath string) {
	defaultImg := "projects/ubuntu-os-cloud/global/images/family/ubuntu-1604-lts"

	// A full image path must match the regex format. If it doesn't, we will fall back to a default base image.
	matches := regexp.MustCompile("projects/(.+)/global/images/(family/)*(.+)").FindStringSubmatch(img)
	if matches != nil {
		// Check to see if the image exists in the given path. The presence of "family" in the path dictates which API call we need to make.
		project, family, name := matches[1], matches[2], matches[3]
		var err error
		if family == "" {
			_, err = gce.computeService.ImagesGet(ctx, project, name)
		} else {
			_, err = gce.computeService.ImagesGetFromFamily(ctx, project, name)
		}

		if err == nil {
			return img
		}
	}

	// Otherwise, fall back to the base image.
	glog.Infof("Could not find image at %s. Defaulting to %s.", img, defaultImg)
	return defaultImg
}

func newDisks(config *gceconfigv1.GCEMachineProviderSpec, zone string, imagePath string, minDiskSizeGb int64) []*compute.AttachedDisk {
	var disks []*compute.AttachedDisk
	for idx, disk := range config.Disks {
		diskSizeGb := disk.InitializeParams.DiskSizeGb
		d := compute.AttachedDisk{
			AutoDelete: true,
			InitializeParams: &compute.AttachedDiskInitializeParams{
				DiskSizeGb: diskSizeGb,
				DiskType:   fmt.Sprintf("zones/%s/diskTypes/%s", zone, disk.InitializeParams.DiskType),
			},
		}
		if idx == 0 {
			d.InitializeParams.SourceImage = imagePath
			d.Boot = true
			if diskSizeGb < minDiskSizeGb {
				glog.Infof("increasing disk size to %v gb, the supplied disk size of %v gb is below the minimum", minDiskSizeGb, diskSizeGb)
				d.InitializeParams.DiskSizeGb = minDiskSizeGb
			}
		}
		disks = append(disks, &d)
	}
	return disks
}

// Just a temporary hack to grab a single range from the config.
func getSubnet(netRange clusterv1.NetworkRanges) string {
	if len(netRange.CIDRBlocks) == 0 {
		return ""
	}
	return netRange.CIDRBlocks[0]
}

func (gce *GCEClient) getKubeadmToken() (string, error) {
	tokenParams := kubeadm.TokenCreateParams{
		Ttl: time.Duration(10) * time.Minute,
	}
	output, err := gce.kubeadm.TokenCreate(tokenParams)
	if err != nil {
		glog.Errorf("unable to create token: %v [%s]", err, output)
		return "", err
	}
	return strings.TrimSpace(output), err
}

func getOrNewKubeadm(params MachineActuatorParams) GCEClientKubeadm {
	if params.Kubeadm == nil {
		return kubeadm.New()
	}
	return params.Kubeadm
}

func getOrNewComputeServiceForMachine(computeService GCEClientComputeService, cloudConfigPath string) (GCEClientComputeService, error) {
	if computeService != nil {
		return computeService, nil
	}

	var client *http.Client
	var err error
	// If specified in the GCE config, use the alternative authentication.
	if cloudConfigPath != "" {
		glog.Info("Trying to get open the GCE config")
		client, err = clientWithAltTokenSource(cloudConfigPath)
		if err != nil {
			glog.Fatalf("Error creating an alternative auth client: %q", err)
		}
	} else {
		glog.Info("Using the default GCP client")
		// The default GCP client expects the environment variable
		// GOOGLE_APPLICATION_CREDENTIALS to point to a file with service credentials.
		client, err = google.DefaultClient(context.TODO(), compute.ComputeScope)
		if err != nil {
			return nil, err
		}
	}

	computeService, err = clients.NewComputeService(client)
	if err != nil {
		return nil, err
	}
	return computeService, nil
}

func clientWithAltTokenSource(gceConfigPath string) (*http.Client, error) {
	glog.Info("Trying to get the alt token")
	gceConfig := struct {
		Global struct {
			ProjectID string `gcfg:"project-id"`
			TokenURL  string `gcfg:"token-url"`
			TokenBody string `gcfg:"token-body"`
		}
	}{}
	if err := gcfg.FatalOnly(gcfg.ReadFileInto(&gceConfig, gceConfigPath)); err != nil {
		return nil, err
	}
	tokenSource := clients.NewAltTokenSource(gceConfig.Global.TokenURL, gceConfig.Global.TokenBody)
	client := oauth2.NewClient(context.Background(), tokenSource)
	return client, nil
}

func (gce *GCEClient) getMetadata(ctx context.Context, cluster *clusterv1.Cluster, clusterConfig *gceconfigv1.GCEClusterProviderSpec, machine *clusterv1.Machine, machineConfig *gceconfigv1.GCEMachineProviderSpec, configParams *machinesetup.ConfigParams) (*compute.Metadata, error) {
	var metadataMap map[string]string
	if machine.Spec.Versions.Kubelet == "" {
		return nil, errors.New("invalid master configuration: missing Machine.Spec.Versions.Kubelet")
	}
	machineSetupConfigs, err := gce.machineSetupConfigGetter.GetMachineSetupConfig()
	if err != nil {
		return nil, err
	}
	machineSetupMetadata, err := machineSetupConfigs.GetMetadata(configParams)
	if err != nil {
		return nil, err
	}
	if isMaster(configParams.Roles) {
		if machine.Spec.Versions.ControlPlane == "" {
			return nil, gce.handleMachineError(ctx, machine, apierrors.InvalidMachineConfiguration(
				"invalid master configuration: missing Machine.Spec.Versions.ControlPlane"), createEventAction)
		}
		var err error
		metadataMap, err = masterMetadata(cluster, machine, clusterConfig.Project, &machineSetupMetadata)
		if err != nil {
			return nil, err
		}
		ca := gce.certificateAuthority
		if ca != nil {
			metadataMap["ca-cert"] = base64.StdEncoding.EncodeToString(ca.Certificate)
			metadataMap["ca-key"] = base64.StdEncoding.EncodeToString(ca.PrivateKey)
		}
	} else {
		var err error
		kubeadmToken, err := gce.getKubeadmToken()
		if err != nil {
			return nil, err
		}
		metadataMap, err = nodeMetadata(kubeadmToken, cluster, machine, machineConfig.Project, &machineSetupMetadata)
		if err != nil {
			return nil, err
		}
	}

	{
		var b strings.Builder

		project := clusterConfig.Project

		clusterName := cluster.Name
		nodeTag := clusterName + "-worker"

		network := "default"
		subnetwork := "kubernetes"

		fmt.Fprintf(&b, "[global]\n")
		fmt.Fprintf(&b, "project-id = %s\n", project)
		fmt.Fprintf(&b, "network-name = %s\n", network)
		fmt.Fprintf(&b, "subnetwork-name = %s\n", subnetwork)
		fmt.Fprintf(&b, "node-tags = %s\n", nodeTag)

		metadataMap["cloud-config"] = b.String()
	}

	var metadataItems []*compute.MetadataItems
	for k, v := range metadataMap {
		v := v // rebind scope to avoid loop aliasing below
		metadataItems = append(metadataItems, &compute.MetadataItems{
			Key:   k,
			Value: &v,
		})
	}
	metadata := compute.Metadata{
		Items: metadataItems,
	}
	return &metadata, nil
}

// TODO: We need to change this when we create dedicated service account for apiserver/controller
// pod.
//
func CreateExtApiServerRoleBinding() error {
	return run("kubectl", "create", "rolebinding",
		"-n", "kube-system", "machine-controller", "--role=extension-apiserver-authentication-reader",
		"--serviceaccount=default:default")
}

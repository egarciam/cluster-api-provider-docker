/*
Copyright 2024.

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

package controller

import (
	"context"
	"encoding/base64"
	"fmt"
	"reflect"
	"time"

	"github.com/pkg/errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/remote"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/kind/pkg/cluster/constants"

	infrav1 "github.com/egarciam/cluster-api-provider-docker/api/v1alpha1"
	"github.com/egarciam/cluster-api-provider-docker/pkg/container"
	"github.com/egarciam/cluster-api-provider-docker/pkg/docker"
	corev1 "k8s.io/api/core/v1"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/kubeadm/api/v1beta1"
)

// DockerMachineReconciler reconciles a DockerMachine object
type DockerMachineReconciler struct {
	client.Client
	//Scheme           *runtime.Scheme
	ContainerRuntime container.Runtime
	Tracker          *remote.ClusterCacheTracker
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=dockermachines,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=dockermachines/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=dockermachines/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the DockerMachine object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.17.0/pkg/reconcile

// Our controller will need read RBAC permission for Machines. So add the following to the comment for Reconcile:
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines;machines/status,verbs=get;list;watch

func (r *DockerMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	logger := log.FromContext(ctx)

	// TODO(user): your logic here
	//Add the container runtime information to the context so it can be used later:
	ctx = container.RuntimeInto(ctx, r.ContainerRuntime)

	// Fetch the DockerMachine instance that we are now reconciling
	dockerMachine := &infrav1.DockerMachine{}
	if err := r.Client.Get(ctx, req.NamespacedName, dockerMachine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Fetch the corresponding Machine that owns this DockerMachine, return if the OwnerRef hasn't been set
	// Get the Cluster
	machine, err := util.GetOwnerMachine(ctx, r.Client, dockerMachine.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, err
	}
	if machine == nil {
		logger.Info("Waiting for Machine Controller to set OwnerRef on DockerMachines")
		return ctrl.Result{}, nil
	}

	// Fetch the corresponding Cluster (CAPI labels each infrastructure Machine with the cluster name,
	// and util.GetClusterFromMetadata() uses that information)

	// Get the Cluster
	cluster, err := util.GetClusterFromMetadata(ctx, r.Client, dockerMachine.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cluster == nil {
		logger.Info(fmt.Sprintf("Please associate this machine with a cluster using the label %s: <name of cluster>", clusterv1.ClusterNameLabel))
		return ctrl.Result{}, nil
	}
	logger = logger.WithValues("cluster", cluster.Name)

	dockerCluster := &infrav1.DockerCluster{}
	dockerClusterName := client.ObjectKey{
		Namespace: dockerMachine.Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}

	if err = r.Client.Get(ctx, dockerClusterName, dockerCluster); err != nil {
		logger.Error(err, "failed to get docker cluster")
		return ctrl.Result{}, nil
	}

	patchHelper, err := patch.NewHelper(dockerMachine, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	defer func() {
		if err = patchDockerMachine(ctx, patchHelper, dockerMachine); err != nil && reterr == nil {
			logger.Error(err, "failed to patch dockerMachine")
			reterr = err
		}
	}()

	// return early if he object or Cluster is paused
	if annotations.IsPaused(cluster, dockerMachine) {
		logger.Info("docker Machine or linked cluster is marked as paused. Won't reconcile")
		return ctrl.Result{}, nil
	}

	// Add finalizer first if not exist to avoid the race condition between init and delete
	if !controllerutil.ContainsFinalizer(dockerMachine, infrav1.MachineFinalizer) {
		controllerutil.AddFinalizer(dockerMachine, infrav1.MachineFinalizer)
		return ctrl.Result{}, nil
	}

	externalMachine, err := docker.NewMachine(ctx, cluster, machine.Name, nil)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to create helper for managing external machine")
	}

	externalLoadBalancer, err := docker.NewLoadBalancer(ctx, cluster, dockerCluster)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to create helper for managing externalLoadBalancer")
	}

	// Handle deleted machines

	if !dockerMachine.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, cluster, machine, dockerMachine, externalMachine, externalLoadBalancer)
	}

	// Handle non deleted machines
	return r.reconcileNormal(ctx, cluster, machine, dockerMachine, externalMachine, externalLoadBalancer)
}

func patchDockerMachine(ctx context.Context, patchHelper *patch.Helper, dockerMachine *infrav1.DockerMachine) error {
	// Always update the readyCondition by summarizing the state of other conditions.
	// A step counter is added to represent progress during the provisioning process (instead we are hiding the step counter during the deletion process).
	conditions.SetSummary(dockerMachine,
		conditions.WithConditions(
			infrav1.ContainerProvisionedCondition,
			infrav1.BootstrapExecSucceededCondition,
		),
		conditions.WithStepCounterIf(dockerMachine.ObjectMeta.DeletionTimestamp.IsZero() && dockerMachine.Spec.ProviderID == nil),
	)

	//Patch the object, igoring conflicts on the conditions owned by this controller
	return patchHelper.Patch(
		ctx,
		dockerMachine,
		patch.WithOwnedConditions{
			Conditions: []clusterv1.ConditionType{
				clusterv1.ReadyCondition,
				infrav1.ContainerProvisionedCondition,
				infrav1.BootstrapExecSucceededCondition,
			},
		},
	)

}

func (r *DockerMachineReconciler) reconcileNormal(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, dockerMachine *infrav1.DockerMachine,
	externalMachine *docker.Machine, externalLoadBalancer *docker.LoadBalancer) (_ ctrl.Result, retErr error) {
	logger := log.FromContext(ctx)

	// Check if the infra is ready, otherwise return and wait for the object to be uopdated
	if !cluster.Status.InfrastructureReady {
		logger.Info("Waiting for DockerCluster Controller to create cluster infrastructure")
		conditions.MarkFalse(dockerMachine, infrav1.ContainerProvisionedCondition, infrav1.WaitingForClusterInfrastructureReason,
			clusterv1.ConditionSeverityInfo, "")

	}
	// If the machine is already provisioned return
	if dockerMachine.Spec.ProviderID != nil {
		// ensure ready state is set
		// This is required after move, because status is not moed to the target cluster
		dockerMachine.Status.Ready = true
		return ctrl.Result{}, nil
	}

	// Now we need to make sure that the bootstrap data is available. This is provided by the bootstrap provider and essentially
	//  is what enables an empty host to turn into a kubernetes node:

	// Make sure bootstrap data is available and populated
	if machine.Spec.Bootstrap.DataSecretName == nil {
		if !util.IsControlPlaneMachine(machine) && !conditions.IsTrue(cluster, clusterv1.ControlPlaneInitializedCondition) {
			logger.Info("Waiting for the control plane to be initialized")
			return ctrl.Result{}, nil
		}

	}
	// It's finally time to do the real work - get the node-to-be's role and create the Docker container if it doesn't exist:
	role := constants.WorkerNodeRoleValue
	if util.IsControlPlaneMachine(machine) {
		role = constants.ControlPlaneNodeRoleValue
	}

	//Create the machine if not existing yet
	if !externalMachine.Exists() {
		if err := externalMachine.Create(ctx, dockerMachine.Spec.CustomImage, role, machine.Spec.Version,
			docker.FailureDomainLabel(machine.Spec.FailureDomain), nil); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to create worker DockerMachine")
		}
	}
	// If we just created a control plane node, we need to update the load balancer configuration to point to it
	//   if the machine is a control plane update the load balancer configuration
	//   we should only do this once, as reconfiguration more or less ensures
	//   node ref setting fails
	if util.IsControlPlaneMachine(machine) && !dockerMachine.Status.LoadBalancerConfigured {
		if err := externalLoadBalancer.UpdateConfiguration(ctx); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to update DockerCluster.loadbalance configuration")
		}
		dockerMachine.Status.LoadBalancerConfigured = true
	}

	// Now that we have a container up, we need to bootstrap it into a kubernetes node. This takes the bootstrap data
	// (either in cloud-init or ignition format), converts it into a series of commands, and executes them in the container.
	// Virtualizations and cloud platforms can generally just accept the bootstrap data upon VM creation and run it upon
	// boot.

	// if the machine isn't bootstrapped, only then run bootstrap scripts
	if !dockerMachine.Spec.Bootstrapped {
		timeoutCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		if err := externalMachine.CheckForBootstrapSuccess(timeoutCtx, false); err != nil {
			bootstrapData, format, err := r.getBootstrapData(timeoutCtx, machine)
			if err != nil {
				logger.Error(err, "failed to get bootstrap data")
				return ctrl.Result{}, err
			}
			// Run the bootstrap script. Simulate cloud-init / ignition
			if err := externalMachine.ExecBootstrap(timeoutCtx, bootstrapData, format); err != nil {
				conditions.MarkFalse(dockerMachine, infrav1.BootstrapExecSucceededCondition, infrav1.BootstrapFailedReason,
					clusterv1.ConditionSeverityWarning, "Repeating bootstrap")
				return ctrl.Result{}, errors.Wrap(err, "failed to exceute DockerMachine bootstrap")
			}
			//check for bootstrap success
			if err := externalMachine.CheckForBootstrapSuccess(timeoutCtx, true); err != nil {
				conditions.MarkFalse(dockerMachine, infrav1.BootstrapExecSucceededCondition, infrav1.BootstrapFailedReason,
					clusterv1.ConditionSeverityWarning, "Repeting bootstrap")
				return ctrl.Result{}, errors.Wrap(err, "failed to check for existance of bootstrap success file at /run/cluster-api/bootstrap-success.complete")
			}
		}

		dockerMachine.Spec.Bootstrapped = true
		// Now report the node's IP addresses:
		if err := setMachineAddress(ctx, dockerMachine, externalMachine); err != nil {
			logger.Error(err, "failed to set the machine address")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

	}

	// Now we need to set the ProviderID both in the spec of the `DockerMachine` and in the spec of the `Node` in the workload cluster. This is necessary because some parts of kubernetes rely on being able to match `Node`s and `Machine`s, such as autoscaling and CSR approval. We add the following code to our **reconcileNormal** function:
	// ```go
	// Usually a cloud provider will do this, but there is no docker-cloud provider
	remoteClient, err := r.Tracker.GetClient(ctx, client.ObjectKeyFromObject(cluster))
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to generate workload cluster client")
	}

	if err := externalMachine.SetNodeProviderID(ctx, remoteClient); err != nil {
		if errors.As(err, &docker.ContainerNotRunningError{}) {
			return ctrl.Result{}, errors.Wrap(err, "failed to patch the kubernetes node with the machine providerID")
		}
		logger.Error(err, "failed to patch the kubernetes node with the machine providerID")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Set ProviderID so the Cluster API Machine Controller can pull it
	providerID := externalMachine.ProviderID()
	dockerMachine.Spec.ProviderID = &providerID

	// Now that we're finally done, we can notify CAPI:
	dockerMachine.Status.Ready = true
	return ctrl.Result{}, nil
}

// The reconcileDelete function needs to undo what reconcileNormal did - mainly delete the container and remove the
// load balancer configuration:
func (r *DockerMachineReconciler) reconcileDelete(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, dockerMachine *infrav1.DockerMachine,
	externalMachine *docker.Machine, externalLoadBalancer *docker.LoadBalancer) (_ ctrl.Result, retErr error) {
	logger := log.FromContext(ctx)

	// Set the ContainerProvisionedCondition reporting delete is started, and issue a patch in order to make
	// this visible to the users.
	patchHelper, err := patch.NewHelper(dockerMachine, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	conditions.MarkFalse(dockerMachine, infrav1.ContainerProvisionedCondition, clusterv1.DeletingReason, clusterv1.ConditionSeverityInfo, "")

	if err = patchDockerMachine(ctx, patchHelper, dockerMachine); err != nil && retErr == nil {
		logger.Error(err, "failed to patch dockerMachine")
		retErr = err
	}

	// delete the machine
	if err := externalMachine.Delete(ctx); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to delete DockerMachine")
	}

	// Update the load balancer configuration
	if util.IsControlPlaneMachine(machine) {
		if err := externalLoadBalancer.UpdateConfiguration(ctx); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to update DockerCluster.loadbalancer configuration")
		}
	}
	// We're done, so delete the finalizer (so that the DockerMachine can be cleaned up) and return:

	controllerutil.RemoveFinalizer(dockerMachine, infrav1.MachineFinalizer)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DockerMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	var (
		controlledType     = &infrav1.DockerMachine{}
		controlledTypeName = reflect.TypeOf(controlledType).Elem().Name()
		controlledTypeGVK  = infrav1.GroupVersion.WithKind(controlledTypeName)
	)
	return ctrl.NewControllerManagedBy(mgr).
		For(controlledType).
		// Watch the CAPI resource that owns this infrastructure resource
		Watches(
			//&source.Kind{&clusterv1.Machine{}},
			&clusterv1.Machine{},
			handler.EnqueueRequestsFromMapFunc(util.MachineToInfrastructureMapFunc(controlledTypeGVK)),
		).
		Complete(r)
}

func (r *DockerMachineReconciler) getBootstrapData(ctx context.Context, machine *clusterv1.Machine) (string, bootstrapv1.Format, error) {
	if machine.Spec.Bootstrap.DataSecretName == nil {
		return "", "", errors.New("error retrieving bootstrap data: linked Machine bootstrao.dataSecretName is nil")
	}

	s := &corev1.Secret{}
	key := client.ObjectKey{Namespace: machine.GetNamespace(), Name: *machine.Spec.Bootstrap.DataSecretName}

	if err := r.Client.Get(ctx, key, s); err != nil {
		return "", "", errors.Wrapf(err, "failed to retrieve bootstrap data secret for DockerMachine %s", klog.KObj(machine))
	}

	value, ok := s.Data["value"]
	if !ok {
		return "", "", errors.New("error retrieving bootstrap data: secret value key is missing")
	}

	format := s.Data["format"]
	if len(format) == 0 {
		format = []byte(bootstrapv1.CloudConfig)
	}

	return base64.StdEncoding.EncodeToString(value), bootstrapv1.Format(format), nil
}

func setMachineAddress(ctx context.Context, dockerMachine *infrav1.DockerMachine, externalMachine *docker.Machine) error {
	machineAddress, err := externalMachine.Address(ctx)
	if err != nil {
		return err
	}

	dockerMachine.Status.Addresses = []clusterv1.MachineAddress{
		{
			Type:    clusterv1.MachineHostName,
			Address: externalMachine.ContainerName(),
		},
		{
			Type:    clusterv1.MachineInternalIP,
			Address: machineAddress,
		},
		{
			Type:    clusterv1.MachineExternalIP,
			Address: machineAddress,
		},
	}
	return nil
}

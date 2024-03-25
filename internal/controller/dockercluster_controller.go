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

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	infrav1 "github.com/egarciam/cluster-api-provider-docker/api/v1alpha1"
	"github.com/egarciam/cluster-api-provider-docker/pkg/container"
	"github.com/egarciam/cluster-api-provider-docker/pkg/docker"
	"github.com/pkg/errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// DockerClusterReconciler reconciles a DockerCluster object
type DockerClusterReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	ContainerRuntime container.Runtime
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=dockerclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=dockerclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=dockerclusters/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the DockerCluster object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.17.0/pkg/reconcile
func (r *DockerClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, rerr error) {
	logger := log.FromContext(ctx)

	//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status,verbs=get;list;watch

	//Add the container runtime information to the context so it can be used later:
	ctx = container.RuntimeInto(ctx, r.ContainerRuntime)

	dockerCluster := &infrav1.DockerCluster{}
	if err := r.Client.Get(ctx, req.NamespacedName, dockerCluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Get the Cluster
	cluster, err := util.GetOwnerCluster(ctx, r.Client, dockerCluster.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cluster == nil {
		logger.Info("Waiting for Cluster Controller to set OwnerRef on DockerCluster")
		return ctrl.Result{}, nil
	}

	logger = logger.WithValues("cluster", klog.KObj(cluster))
	ctx = ctrl.LoggerInto(ctx, logger)

	//Reconciliation can be paused, for instance when you pivot from an ephemeral bootstrap cluster to a permanent
	//management cluster (i.e. via clusterctl move).
	//We can check if the reconciliation is paused by looking for an annotation:
	if annotations.IsPaused(cluster, dockerCluster) {
		logger.Info("DockerCluster or owning Cluster is marked as paused, not reconciling")

		return ctrl.Result{}, nil
	}

	// Create a helper for managing a docker container hosting the loadbalancer.
	externalLoadBalancer, err := docker.NewLoadBalancer(ctx, cluster, dockerCluster)
	if err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "failed to create helper for managing the externalLoadBalancer")
	}

	//When we exit reconciliation, we want to persist any changes to DockerCluster and this is done by using a patch helper:
	// Initialize the patch helper
	patchHelper, err := patch.NewHelper(dockerCluster, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Always attempt to Patch the DockerCluster object and status after each reconciliation.
	defer func() {
		if err := patchHelper.Patch(ctx, dockerCluster); err != nil {
			logger.Error(err, "failed to patch DockerCluster")
			if rerr == nil {
				rerr = err
			}
		}
	}()

	// Now we are at the position of being able to do the actions that are specific to the Docker provider for create/update (i.e. reconcileNormal) and delete (i.e. reconcileDelete). Replace return ctrl.Result{}, nil with
	// Handle deleted clusters
	if !dockerCluster.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, dockerCluster, externalLoadBalancer)
	}

	// Handle non-deleted clusters
	return r.reconcileNormal(ctx, dockerCluster, externalLoadBalancer)

}

func (r *DockerClusterReconciler) reconcileNormal(ctx context.Context, dockerCluster *infrav1.DockerCluster, externalLoadBalancer *docker.LoadBalancer) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling DockerCluster")

	// We want to ensure that the finalizer for DockerCluster is added so that if the DockerCluster instance is deleted
	// later on, we get the chance to do any required cleanup:
	if !controllerutil.ContainsFinalizer(dockerCluster, infrav1.ClusterFinalizer) {
		controllerutil.AddFinalizer(dockerCluster, infrav1.ClusterFinalizer)

		return ctrl.Result{Requeue: true}, nil

		// Note the use of return ctrl.Result{Requeue: true}, nil. This means that after we have added the finalizer we will
		// exit reconciliation and our DockerCluster is patched. The Requeue: true will then cause the Reconciliation to be
		// called again without the DockerCluster instance being updated. This is a common pattern to ensure changes are
		// persisted and as such its important to ensure your reconciliation logic is idempotent.
	}
	// Now we can create the load balancer container instances:
	// Create the docker container hosting the load balancer if it does not exist.
	if err := externalLoadBalancer.Create(ctx); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to create load balancer")
	}

	// Now get the IP address of the load balancer container:
	// Get the load balancer IP so we can use it for the enpoint address
	lbIP, err := externalLoadBalancer.IP(ctx)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to get IP for the load balancer")
	}

	// Use the ip address to set the endpoint for the control plane. This is required so that CAPI can use it when creating the kubeconfig for our cluster:
	dockerCluster.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{
		Host: lbIP,
		Port: 6443,
	}

	// Finally set the Ready status property to true to indicate to CAPI that the infrastructure is ready and it
	// can continue with creating the cluster:
	dockerCluster.Status.Ready = true

	return ctrl.Result{}, nil
}

func (r *DockerClusterReconciler) reconcileDelete(ctx context.Context, dockerCluster *infrav1.DockerCluster, externalLoadBalancer *docker.LoadBalancer) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling DockerCluster deletion")

	// Delete any external infrastructure. For our provider, we need to delete the instance of the load balancer container:
	// Delete the docker container hosting the load balancer
	if err := externalLoadBalancer.Delete(ctx); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to delete load balancer")
	}

	// As all the external infrastructure is deleted we can remove the finalizer:
	// Cluster is deleted so remove the finalizer.
	controllerutil.RemoveFinalizer(dockerCluster, infrav1.ClusterFinalizer)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DockerClusterReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {

	// This tells controller runtime to call Reconcile when there is a change to DockerCluster. Additionally you can add
	// predicates (or event filters) to stop reconciliation occurring in certain situations. In this instance we use
	// WithEventFilter(predicates.ResourceNotPaused to ensure reconciliation is not called when reconciliation is paused.
	// You can also customize the settings of the "controller manager" by using WithOptions(options) if needed,
	// this often used to limit the number of concurrent reconciliations (although there will be at most one reconcile
	// for a given instance).
	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.DockerCluster{}).
		//WithOptions(options).
		WithEventFilter(predicates.ResourceNotPaused(ctrl.LoggerFrom(ctx))).
		Build(r)
	if err != nil {
		return err
	}

	// In addition to the controller reconciling on changes to DockerCluster we would also like it to happen if there are changes to its owning Cluster.
	// Controller runtime allows you to watch a different resource type and then decide if you want to enqueue a request for reconciliation. Add the following:
	return c.Watch(
		&source.Kind{Type: &clusterv1.Cluster{}},
		handler.EnqueueRequestsFromMapFunc(util.ClusterToInfrastructureMapFunc(ctx, infrav1.GroupVersion.WithKind("DockerCluster"), mgr.GetClient(), &infrav1.DockerCluster{})),
		predicates.ClusterUnpaused(ctrl.LoggerFrom(ctx)),
	)

	/**
	This is saying to watch clusterv1.Cluster and if there is a change to a Cluster instance, get the child DockerCluster
	name/namespace using util.ClusterToInfrastructureMapFunc(ctx, infrav1.GroupVersion.WithKind("DockerCluster"),
	mgr.GetClient(), &infrav1.DockerCluster{}) and then use that name/namespace to enqueue a request for reconciliation of
	the DockerCluster instance with that name/namespace using handler.EnqueueRequestsFromMapFunc. This will then result in
	Reconciliation being called.
	*/

}

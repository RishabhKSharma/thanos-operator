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
	"fmt"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"

	monitoringthanosiov1alpha1 "github.com/thanos-community/thanos-operator/api/v1alpha1"
	"github.com/thanos-community/thanos-operator/internal/pkg/manifests"
	manifestsstore "github.com/thanos-community/thanos-operator/internal/pkg/manifests/store"
	controllermetrics "github.com/thanos-community/thanos-operator/internal/pkg/metrics"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ThanosStoreReconciler reconciles a ThanosStore object
type ThanosStoreReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	logger logr.Logger

	reg                   prometheus.Registerer
	ControllerBaseMetrics *controllermetrics.BaseMetrics
}

// NewThanosStoreReconciler returns a reconciler for ThanosStore resources.
func NewThanosStoreReconciler(logger logr.Logger, client client.Client, scheme *runtime.Scheme, recorder record.EventRecorder, reg prometheus.Registerer, controllerBaseMetrics *controllermetrics.BaseMetrics) *ThanosStoreReconciler {
	return &ThanosStoreReconciler{
		Client:   client,
		Scheme:   scheme,
		Recorder: recorder,

		logger:                logger,
		reg:                   reg,
		ControllerBaseMetrics: controllerBaseMetrics,
	}
}

//+kubebuilder:rbac:groups=monitoring.thanos.io,resources=thanosstores,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=monitoring.thanos.io,resources=thanosstores/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=monitoring.thanos.io,resources=thanosstores/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=services;configmaps;serviceaccounts,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.17.0/pkg/reconcile
func (r *ThanosStoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.ControllerBaseMetrics.ReconciliationsTotal.WithLabelValues(manifestsstore.Name).Inc()

	store := &monitoringthanosiov1alpha1.ThanosStore{}
	err := r.Get(ctx, req.NamespacedName, store)
	if err != nil {
		r.ControllerBaseMetrics.ClientErrorsTotal.WithLabelValues(manifestsstore.Name).Inc()
		if apierrors.IsNotFound(err) {
			r.logger.Info("thanos store resource not found. ignoring since object may be deleted")
			return ctrl.Result{}, nil
		}
		r.logger.Error(err, "failed to get ThanosStore")
		r.ControllerBaseMetrics.ReconciliationsFailedTotal.WithLabelValues(manifestsstore.Name).Inc()
		r.Recorder.Event(store, corev1.EventTypeWarning, "GetFailed", "Failed to get ThanosStore resource")
		return ctrl.Result{}, err
	}

	if store.Spec.Paused != nil {
		if *store.Spec.Paused {
			r.logger.Info("reconciliation is paused for ThanosStore")
			r.Recorder.Event(store, corev1.EventTypeNormal, "Paused", "Reconciliation is paused for ThanosStore resource")
			return ctrl.Result{}, nil
		}
	}

	err = r.syncResources(ctx, *store)
	if err != nil {
		r.ControllerBaseMetrics.ReconciliationsFailedTotal.WithLabelValues(manifestsstore.Name).Inc()
		r.Recorder.Event(store, corev1.EventTypeWarning, "SyncFailed", fmt.Sprintf("Failed to sync resources: %v", err))
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *ThanosStoreReconciler) syncResources(ctx context.Context, store monitoringthanosiov1alpha1.ThanosStore) error {
	var errCount int32
	shardedObjects := r.buildStore(store)

	// todo - we need to prune any orphaned resources here at this point

	for _, shardObjs := range shardedObjects {
		for _, obj := range shardObjs {
			if manifests.IsNamespacedResource(obj) {
				obj.SetNamespace(store.GetNamespace())
				if err := ctrl.SetControllerReference(&store, obj, r.Scheme); err != nil {
					r.logger.Error(err, "failed to set controller owner reference to resource")
					errCount++
					continue
				}
			}

			desired := obj.DeepCopyObject().(client.Object)
			mutateFn := manifests.MutateFuncFor(obj, desired)

			op, err := ctrl.CreateOrUpdate(ctx, r.Client, obj, mutateFn)
			if err != nil {
				r.logger.Error(
					err, "failed to create or update resource",
					"gvk", obj.GetObjectKind().GroupVersionKind().String(),
					"resource", obj.GetName(),
					"namespace", obj.GetNamespace(),
				)
				errCount++
				continue
			}

			r.logger.V(1).Info(
				"resource configured",
				"operation", op, "gvk", obj.GetObjectKind().GroupVersionKind().String(),
				"resource", obj.GetName(), "namespace", obj.GetNamespace(),
			)
		}
	}

	if errCount > 0 {
		r.ControllerBaseMetrics.ClientErrorsTotal.WithLabelValues(manifestsstore.Name).Add(float64(errCount))
		return fmt.Errorf("failed to create or update %d resources for store or store shard(s)", errCount)
	}

	return nil
}

// buildStore returns a map of slices of client.Object that represents the desired state of the Thanos Store resources.
// each key represents a named shard and each slice value represents the resources for that shard.
func (r *ThanosStoreReconciler) buildStore(store monitoringthanosiov1alpha1.ThanosStore) map[string][]client.Object {
	opts := r.specToOptions(store)
	shardedObjects := make(map[string][]client.Object, len(opts))

	for i, opt := range opts {
		storeObjs := manifestsstore.Build(opt)
		shardedObjects[i] = append(shardedObjects[i], storeObjs...)
	}
	return shardedObjects
}

func (r *ThanosStoreReconciler) specToOptions(store monitoringthanosiov1alpha1.ThanosStore) map[string]manifestsstore.Options {
	// build the shared options
	opts := r.specToManifestOptions(store)

	// buildOpts returns the Options for a Thanos Store.
	buildOpts := func() manifestsstore.Options {
		return manifestsstore.Options{
			ObjStoreSecret:           store.Spec.ObjectStorageConfig.ToSecretKeySelector(),
			IndexCacheConfig:         store.Spec.IndexCacheConfig,
			CachingBucketConfig:      store.Spec.CachingBucketConfig,
			Min:                      manifests.Duration(manifests.OptionalToString(store.Spec.MinTime)),
			Max:                      manifests.Duration(manifests.OptionalToString(store.Spec.MaxTime)),
			IgnoreDeletionMarksDelay: manifests.Duration(store.Spec.IgnoreDeletionMarksDelay),
			StorageSize:              resource.MustParse(string(store.Spec.StorageSize)),
			Options:                  opts,
		}
	}
	// no sharding strategy, or sharding strategy with 1 shard, return a single store
	if store.Spec.ShardingStrategy.Shards == 0 || store.Spec.ShardingStrategy.Shards == 1 {
		return map[string]manifestsstore.Options{
			store.GetName(): buildOpts(),
		}
	}

	shardCount := int(store.Spec.ShardingStrategy.Shards)
	shardedOptions := make(map[string]manifestsstore.Options, shardCount)

	for i := range store.Spec.ShardingStrategy.Shards {
		shardName := storeShardName(store.GetName(), i)
		storeShardOpts := buildOpts()
		storeShardOpts.ShardName = shardName
		storeShardOpts.RelabelConfigs = manifests.RelabelConfigs{
			{
				Action:      "hashmod",
				SourceLabel: "__block_id",
				TargetLabel: "shard",
				Modulus:     shardCount,
			},
			{
				Action:      "keep",
				SourceLabel: "shard",
				Regex:       fmt.Sprintf("%d", i),
			},
		}
		shardedOptions[shardName] = storeShardOpts
	}
	return shardedOptions
}

func (r *ThanosStoreReconciler) specToManifestOptions(store monitoringthanosiov1alpha1.ThanosStore) manifests.Options {
	return manifests.Options{
		Name:                 store.GetName(),
		Namespace:            store.GetNamespace(),
		Replicas:             store.Spec.ShardingStrategy.ShardReplicas,
		Labels:               manifests.MergeLabels(store.GetLabels(), store.Spec.Labels),
		Image:                store.Spec.Image,
		LogLevel:             store.Spec.LogLevel,
		LogFormat:            store.Spec.LogFormat,
		ResourceRequirements: store.Spec.ResourceRequirements,
		Additional: manifests.Additional{
			Args:         store.Spec.Args,
			Containers:   store.Spec.Containers,
			Env:          store.Spec.Env,
			Volumes:      store.Spec.Volumes,
			VolumeMounts: store.Spec.VolumeMounts,
			Ports:        store.Spec.Ports,
			ServicePorts: store.Spec.ServicePorts,
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ThanosStoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&monitoringthanosiov1alpha1.ThanosStore{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.Service{}).
		Owns(&appsv1.StatefulSet{}).
		Complete(r)

	if err != nil {
		r.Recorder.Event(&monitoringthanosiov1alpha1.ThanosStore{}, corev1.EventTypeWarning, "SetupFailed", fmt.Sprintf("Failed to set up controller: %v", err))
		return err
	}

	return nil
}

// storeShardName generates name for a Thanos Store shard.
func storeShardName(parentName string, shardIndex int32) string {
	name := fmt.Sprintf("%s-shard-%d", parentName, shardIndex)
	// todo - figure out a strategy for global naming for all resources
	return name
}

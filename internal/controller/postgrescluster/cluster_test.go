// +build envtest

package postgrescluster

/*
 Copyright 2021 Crunchy Data Solutions, Inc.
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

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/crunchydata/postgres-operator/internal/initialize"
	"github.com/crunchydata/postgres-operator/internal/naming"
	"github.com/crunchydata/postgres-operator/pkg/apis/postgres-operator.crunchydata.com/v1beta1"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel"
	"gotest.tools/v3/assert"
	appsv1 "k8s.io/api/apps/v1"
	batchv1beta1 "k8s.io/api/batch/v1beta1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var gvks = []schema.GroupVersionKind{{
	Group:   v1.SchemeGroupVersion.Group,
	Version: v1.SchemeGroupVersion.Version,
	Kind:    "ConfigMapList",
}, {
	Group:   v1.SchemeGroupVersion.Group,
	Version: v1.SchemeGroupVersion.Version,
	Kind:    "SecretList",
}, {
	Group:   appsv1.SchemeGroupVersion.Group,
	Version: appsv1.SchemeGroupVersion.Version,
	Kind:    "StatefulSetList",
}, {
	Group:   appsv1.SchemeGroupVersion.Group,
	Version: appsv1.SchemeGroupVersion.Version,
	Kind:    "DeploymentList",
}, {
	Group:   batchv1beta1.SchemeGroupVersion.Group,
	Version: batchv1beta1.SchemeGroupVersion.Version,
	Kind:    "CronJobList",
}, {
	Group:   v1.SchemeGroupVersion.Group,
	Version: v1.SchemeGroupVersion.Version,
	Kind:    "PersistentVolumeClaimList",
}, {
	Group:   v1.SchemeGroupVersion.Group,
	Version: v1.SchemeGroupVersion.Version,
	Kind:    "ServiceList",
}, {
	Group:   v1.SchemeGroupVersion.Group,
	Version: v1.SchemeGroupVersion.Version,
	Kind:    "EndpointsList",
}, {
	Group:   v1.SchemeGroupVersion.Group,
	Version: v1.SchemeGroupVersion.Version,
	Kind:    "ServiceAccountList",
}, {
	Group:   rbacv1.SchemeGroupVersion.Group,
	Version: rbacv1.SchemeGroupVersion.Version,
	Kind:    "RoleBindingList",
}, {
	Group:   rbacv1.SchemeGroupVersion.Group,
	Version: rbacv1.SchemeGroupVersion.Version,
	Kind:    "RoleList",
}}

func TestCustomLabels(t *testing.T) {
	t.Parallel()

	env, cc, config := setupTestEnv(t, ControllerName)
	t.Cleanup(func() { teardownTestEnv(t, env) })

	reconciler := &Reconciler{}
	ctx, cancel := setupManager(t, config, func(mgr manager.Manager) {
		reconciler = &Reconciler{
			Client:   cc,
			Owner:    client.FieldOwner(t.Name()),
			Recorder: mgr.GetEventRecorderFor(ControllerName),
			Tracer:   otel.Tracer(t.Name()),
		}
	})
	t.Cleanup(func() { teardownManager(cancel, t) })

	ns := &v1.Namespace{}
	ns.GenerateName = "postgres-operator-test-"
	ns.Labels = labels.Set{"postgres-operator-test": t.Name()}
	assert.NilError(t, cc.Create(ctx, ns))
	t.Cleanup(func() { assert.Check(t, cc.Delete(ctx, ns)) })

	reconcileTestCluster := func(cluster *v1beta1.PostgresCluster) {
		assert.NilError(t, errors.WithStack(reconciler.Client.Create(ctx, cluster)))
		t.Cleanup(func() {
			// Remove finalizers, if any, so the namespace can terminate.
			assert.Check(t, client.IgnoreNotFound(
				reconciler.Client.Patch(ctx, cluster, client.RawPatch(
					client.Merge.Type(), []byte(`{"metadata":{"finalizers":[]}}`)))))
		})

		// Reconcile the cluster
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(cluster),
		})
		assert.NilError(t, err)
		assert.Assert(t, result.Requeue == false)
	}

	getUnstructuredLabels := func(cluster v1beta1.PostgresCluster, u unstructured.Unstructured) (map[string]map[string]string, error) {
		var err error
		labels := map[string]map[string]string{}

		if metav1.IsControlledBy(&u, &cluster) {
			switch u.GetKind() {
			case "StatefulSet":
				var resource appsv1.StatefulSet
				err = runtime.DefaultUnstructuredConverter.
					FromUnstructured(u.UnstructuredContent(), &resource)
				labels["resource"] = resource.GetLabels()
				labels["podTemplate"] = resource.Spec.Template.GetLabels()
			case "Deployment":
				var resource appsv1.Deployment
				err = runtime.DefaultUnstructuredConverter.
					FromUnstructured(u.UnstructuredContent(), &resource)
				labels["resource"] = resource.GetLabels()
				labels["podTemplate"] = resource.Spec.Template.GetLabels()
			case "CronJob":
				var resource batchv1beta1.CronJob
				err = runtime.DefaultUnstructuredConverter.
					FromUnstructured(u.UnstructuredContent(), &resource)
				labels["resource"] = resource.GetLabels()
				labels["jobTemplate"] = resource.Spec.JobTemplate.GetLabels()
				labels["jobPodTemplate"] = resource.Spec.JobTemplate.Spec.Template.GetLabels()
			default:
				labels["resource"] = u.GetLabels()
			}
		}
		return labels, err
	}

	t.Run("Cluster", func(t *testing.T) {
		cluster := testCluster()
		cluster.ObjectMeta.Name = "global-cluster"
		cluster.ObjectMeta.Namespace = ns.Name
		cluster.Spec.InstanceSets = []v1beta1.PostgresInstanceSetSpec{{
			Name:                "daisy-instance1",
			Replicas:            Int32(1),
			DataVolumeClaimSpec: testVolumeClaimSpec(),
		}, {
			Name:                "daisy-instance2",
			Replicas:            Int32(1),
			DataVolumeClaimSpec: testVolumeClaimSpec(),
		}}
		cluster.Spec.Metadata = &v1beta1.Metadata{
			Labels: map[string]string{"my.cluster.label": "daisy"},
		}
		testCronSchedule := "@yearly"
		cluster.Spec.Backups.PGBackRest.Repos[0].BackupSchedules = &v1beta1.PGBackRestBackupSchedules{
			Full:         &testCronSchedule,
			Differential: &testCronSchedule,
			Incremental:  &testCronSchedule,
		}
		selector, err := naming.AsSelector(metav1.LabelSelector{
			MatchLabels: map[string]string{
				naming.LabelCluster: cluster.Name,
			},
		})
		assert.NilError(t, err)
		reconcileTestCluster(cluster)

		for _, gvk := range gvks {
			uList := &unstructured.UnstructuredList{}
			uList.SetGroupVersionKind(gvk)
			assert.NilError(t, reconciler.Client.List(ctx, uList,
				client.InNamespace(cluster.Namespace),
				client.MatchingLabelsSelector{Selector: selector}))

			for i := range uList.Items {
				u := uList.Items[i]
				labels, err := getUnstructuredLabels(*cluster, u)
				assert.NilError(t, err)
				for resourceType, resourceLabels := range labels {
					t.Run(u.GetKind()+"/"+u.GetName()+"/"+resourceType, func(t *testing.T) {
						assert.Equal(t, resourceLabels["my.cluster.label"], "daisy")
					})
				}
			}
		}
	})

	t.Run("Instance", func(t *testing.T) {
		cluster := testCluster()
		cluster.ObjectMeta.Name = "instance-cluster"
		cluster.ObjectMeta.Namespace = ns.Name
		cluster.Spec.InstanceSets = []v1beta1.PostgresInstanceSetSpec{{
			Name:                "max-instance",
			Replicas:            Int32(1),
			DataVolumeClaimSpec: testVolumeClaimSpec(),
			Metadata: &v1beta1.Metadata{
				Labels: map[string]string{"my.instance.label": "max"},
			},
		}, {
			Name:                "lucy-instance",
			Replicas:            Int32(1),
			DataVolumeClaimSpec: testVolumeClaimSpec(),
			Metadata: &v1beta1.Metadata{
				Labels: map[string]string{"my.instance.label": "lucy"},
			},
		}}
		reconcileTestCluster(cluster)
		for _, set := range cluster.Spec.InstanceSets {
			t.Run(set.Name, func(t *testing.T) {
				selector, err := naming.AsSelector(metav1.LabelSelector{
					MatchLabels: map[string]string{
						naming.LabelCluster:     cluster.Name,
						naming.LabelInstanceSet: set.Name,
					},
				})
				assert.NilError(t, err)

				for _, gvk := range gvks {
					uList := &unstructured.UnstructuredList{}
					uList.SetGroupVersionKind(gvk)
					assert.NilError(t, reconciler.Client.List(ctx, uList,
						client.InNamespace(cluster.Namespace),
						client.MatchingLabelsSelector{Selector: selector}))

					for i := range uList.Items {
						u := uList.Items[i]

						labels, err := getUnstructuredLabels(*cluster, u)
						assert.NilError(t, err)
						for resourceType, resourceLabels := range labels {
							t.Run(u.GetKind()+"/"+u.GetName()+"/"+resourceType, func(t *testing.T) {
								assert.Equal(t, resourceLabels["my.instance.label"], set.Metadata.Labels["my.instance.label"])
							})
						}
					}
				}
			})
		}

	})

	t.Run("PGBackRest", func(t *testing.T) {
		cluster := testCluster()
		cluster.ObjectMeta.Name = "pgbackrest-cluster"
		cluster.ObjectMeta.Namespace = ns.Name
		cluster.Spec.Backups.PGBackRest.Metadata = &v1beta1.Metadata{
			Labels: map[string]string{"my.pgbackrest.label": "lucy"},
		}
		testCronSchedule := "@yearly"
		cluster.Spec.Backups.PGBackRest.Repos[0].BackupSchedules = &v1beta1.PGBackRestBackupSchedules{
			Full:         &testCronSchedule,
			Differential: &testCronSchedule,
			Incremental:  &testCronSchedule,
		}
		reconcileTestCluster(cluster)

		selector, err := naming.AsSelector(metav1.LabelSelector{
			MatchLabels: map[string]string{
				naming.LabelCluster: cluster.Name,
			},
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      naming.LabelPGBackRest,
				Operator: metav1.LabelSelectorOpExists},
			},
		})
		assert.NilError(t, err)

		for _, gvk := range gvks {
			uList := &unstructured.UnstructuredList{}
			uList.SetGroupVersionKind(gvk)
			assert.NilError(t, reconciler.Client.List(ctx, uList,
				client.InNamespace(cluster.Namespace),
				client.MatchingLabelsSelector{Selector: selector}))

			for i := range uList.Items {
				u := uList.Items[i]

				labels, err := getUnstructuredLabels(*cluster, u)
				assert.NilError(t, err)
				for resourceType, resourceLabels := range labels {
					t.Run(u.GetKind()+"/"+u.GetName()+"/"+resourceType, func(t *testing.T) {
						assert.Equal(t, resourceLabels["my.pgbackrest.label"], "lucy")
					})
				}
			}
		}
	})

	t.Run("PGBouncer", func(t *testing.T) {
		cluster := testCluster()
		cluster.ObjectMeta.Name = "pgbouncer-cluster"
		cluster.ObjectMeta.Namespace = ns.Name
		cluster.Spec.Proxy.PGBouncer.Metadata = &v1beta1.Metadata{
			Labels: map[string]string{"my.pgbouncer.label": "lucy"},
		}
		reconcileTestCluster(cluster)

		selector, err := naming.AsSelector(metav1.LabelSelector{
			MatchLabels: map[string]string{
				naming.LabelCluster: cluster.Name,
				naming.LabelRole:    naming.RolePGBouncer,
			},
		})
		assert.NilError(t, err)

		for _, gvk := range gvks {
			uList := &unstructured.UnstructuredList{}
			uList.SetGroupVersionKind(gvk)
			assert.NilError(t, reconciler.Client.List(ctx, uList,
				client.InNamespace(cluster.Namespace),
				client.MatchingLabelsSelector{Selector: selector}))

			for i := range uList.Items {
				u := uList.Items[i]

				labels, err := getUnstructuredLabels(*cluster, u)
				assert.NilError(t, err)
				for resourceType, resourceLabels := range labels {
					t.Run(u.GetKind()+"/"+u.GetName()+"/"+resourceType, func(t *testing.T) {
						assert.Equal(t, resourceLabels["my.pgbouncer.label"], "lucy")
					})
				}
			}
		}
	})
}

func TestCustomAnnotations(t *testing.T) {
	t.Parallel()

	env, cc, config := setupTestEnv(t, ControllerName)
	t.Cleanup(func() { teardownTestEnv(t, env) })

	reconciler := &Reconciler{}
	ctx, cancel := setupManager(t, config, func(mgr manager.Manager) {
		reconciler = &Reconciler{
			Client:   cc,
			Owner:    client.FieldOwner(t.Name()),
			Recorder: mgr.GetEventRecorderFor(ControllerName),
			Tracer:   otel.Tracer(t.Name()),
		}
	})
	t.Cleanup(func() { teardownManager(cancel, t) })

	ns := &v1.Namespace{}
	ns.GenerateName = "postgres-operator-test-"
	ns.Labels = labels.Set{"postgres-operator-test": ""}
	assert.NilError(t, cc.Create(ctx, ns))
	t.Cleanup(func() { assert.Check(t, cc.Delete(ctx, ns)) })

	reconcileTestCluster := func(cluster *v1beta1.PostgresCluster) {
		assert.NilError(t, errors.WithStack(reconciler.Client.Create(ctx, cluster)))
		t.Cleanup(func() {
			// Remove finalizers, if any, so the namespace can terminate.
			assert.Check(t, client.IgnoreNotFound(
				reconciler.Client.Patch(ctx, cluster, client.RawPatch(
					client.Merge.Type(), []byte(`{"metadata":{"finalizers":[]}}`)))))
		})

		// Reconcile the cluster
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(cluster),
		})
		assert.NilError(t, err)
		assert.Assert(t, result.Requeue == false)
	}

	getUnstructuredAnnotations := func(cluster v1beta1.PostgresCluster, u unstructured.Unstructured) (map[string]map[string]string, error) {
		var err error
		annotations := map[string]map[string]string{}

		if metav1.IsControlledBy(&u, &cluster) {
			switch u.GetKind() {
			case "StatefulSet":
				var resource appsv1.StatefulSet
				err = runtime.DefaultUnstructuredConverter.
					FromUnstructured(u.UnstructuredContent(), &resource)
				annotations["resource"] = resource.GetAnnotations()
				annotations["podTemplate"] = resource.Spec.Template.GetAnnotations()
			case "Deployment":
				var resource appsv1.Deployment
				err = runtime.DefaultUnstructuredConverter.
					FromUnstructured(u.UnstructuredContent(), &resource)
				annotations["resource"] = resource.GetAnnotations()
				annotations["podTemplate"] = resource.Spec.Template.GetAnnotations()
			case "CronJob":
				var resource batchv1beta1.CronJob
				err = runtime.DefaultUnstructuredConverter.
					FromUnstructured(u.UnstructuredContent(), &resource)
				annotations["resource"] = resource.GetAnnotations()
				annotations["jobTemplate"] = resource.Spec.JobTemplate.GetAnnotations()
				annotations["jobPodTemplate"] = resource.Spec.JobTemplate.Spec.Template.GetAnnotations()
			default:
				annotations["resource"] = u.GetAnnotations()
			}
		}
		return annotations, err
	}

	t.Run("Cluster", func(t *testing.T) {
		cluster := testCluster()
		cluster.ObjectMeta.Name = "global-cluster"
		cluster.ObjectMeta.Namespace = ns.Name
		cluster.Spec.InstanceSets = []v1beta1.PostgresInstanceSetSpec{{
			Name:                "daisy-instance1",
			Replicas:            Int32(1),
			DataVolumeClaimSpec: testVolumeClaimSpec(),
		}, {
			Name:                "daisy-instance2",
			Replicas:            Int32(1),
			DataVolumeClaimSpec: testVolumeClaimSpec(),
		}}
		cluster.Spec.Metadata = &v1beta1.Metadata{
			Annotations: map[string]string{"my.cluster.annotation": "daisy"},
		}
		testCronSchedule := "@yearly"
		cluster.Spec.Backups.PGBackRest.Repos[0].BackupSchedules = &v1beta1.PGBackRestBackupSchedules{
			Full:         &testCronSchedule,
			Differential: &testCronSchedule,
			Incremental:  &testCronSchedule,
		}
		reconcileTestCluster(cluster)

		selector, err := naming.AsSelector(metav1.LabelSelector{
			MatchLabels: map[string]string{
				naming.LabelCluster: cluster.Name,
			},
		})
		assert.NilError(t, err)

		for _, gvk := range gvks {
			uList := &unstructured.UnstructuredList{}
			uList.SetGroupVersionKind(gvk)
			assert.NilError(t, reconciler.Client.List(ctx, uList,
				client.InNamespace(cluster.Namespace),
				client.MatchingLabelsSelector{Selector: selector}))

			for i := range uList.Items {
				u := uList.Items[i]
				annotations, err := getUnstructuredAnnotations(*cluster, u)
				assert.NilError(t, err)
				for resourceType, resourceAnnotations := range annotations {
					t.Run(u.GetKind()+"/"+u.GetName()+"/"+resourceType, func(t *testing.T) {
						assert.Equal(t, resourceAnnotations["my.cluster.annotation"], "daisy")
					})
				}
			}
		}
	})

	t.Run("Instance", func(t *testing.T) {
		cluster := testCluster()
		cluster.ObjectMeta.Name = "instance-cluster"
		cluster.ObjectMeta.Namespace = ns.Name
		cluster.Spec.InstanceSets = []v1beta1.PostgresInstanceSetSpec{{
			Name:                "max-instance",
			Replicas:            Int32(1),
			DataVolumeClaimSpec: testVolumeClaimSpec(),
			Metadata: &v1beta1.Metadata{
				Annotations: map[string]string{"my.instance.annotation": "max"},
			},
		}, {
			Name:                "lucy-instance",
			Replicas:            Int32(1),
			DataVolumeClaimSpec: testVolumeClaimSpec(),
			Metadata: &v1beta1.Metadata{
				Annotations: map[string]string{"my.instance.annotation": "lucy"},
			},
		}}
		reconcileTestCluster(cluster)
		for _, set := range cluster.Spec.InstanceSets {
			t.Run(set.Name, func(t *testing.T) {
				selector, err := naming.AsSelector(metav1.LabelSelector{
					MatchLabels: map[string]string{
						naming.LabelCluster:     cluster.Name,
						naming.LabelInstanceSet: set.Name,
					},
				})
				assert.NilError(t, err)

				for _, gvk := range gvks {
					uList := &unstructured.UnstructuredList{}
					uList.SetGroupVersionKind(gvk)
					assert.NilError(t, reconciler.Client.List(ctx, uList,
						client.InNamespace(cluster.Namespace),
						client.MatchingLabelsSelector{Selector: selector}))

					for i := range uList.Items {
						u := uList.Items[i]

						annotations, err := getUnstructuredAnnotations(*cluster, u)
						assert.NilError(t, err)
						for resourceType, resourceAnnotations := range annotations {
							t.Run(u.GetKind()+"/"+u.GetName()+"/"+resourceType, func(t *testing.T) {
								assert.Equal(t, resourceAnnotations["my.instance.annotation"], set.Metadata.Annotations["my.instance.annotation"])
							})
						}
					}
				}
			})
		}

	})

	t.Run("PGBackRest", func(t *testing.T) {
		cluster := testCluster()
		cluster.ObjectMeta.Name = "pgbackrest-cluster"
		cluster.ObjectMeta.Namespace = ns.Name
		cluster.Spec.Backups.PGBackRest.Metadata = &v1beta1.Metadata{
			Annotations: map[string]string{"my.pgbackrest.annotation": "lucy"},
		}
		testCronSchedule := "@yearly"
		cluster.Spec.Backups.PGBackRest.Repos[0].BackupSchedules = &v1beta1.PGBackRestBackupSchedules{
			Full:         &testCronSchedule,
			Differential: &testCronSchedule,
			Incremental:  &testCronSchedule,
		}
		reconcileTestCluster(cluster)

		selector, err := naming.AsSelector(metav1.LabelSelector{
			MatchLabels: map[string]string{
				naming.LabelCluster: cluster.Name,
			},
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      naming.LabelPGBackRest,
				Operator: metav1.LabelSelectorOpExists},
			},
		})
		assert.NilError(t, err)

		for _, gvk := range gvks {
			uList := &unstructured.UnstructuredList{}
			uList.SetGroupVersionKind(gvk)
			assert.NilError(t, reconciler.Client.List(ctx, uList,
				client.InNamespace(cluster.Namespace),
				client.MatchingLabelsSelector{Selector: selector}))

			for i := range uList.Items {
				u := uList.Items[i]

				annotations, err := getUnstructuredAnnotations(*cluster, u)
				assert.NilError(t, err)
				for resourceType, resourceAnnotations := range annotations {
					t.Run(u.GetKind()+"/"+u.GetName()+"/"+resourceType, func(t *testing.T) {
						assert.Equal(t, resourceAnnotations["my.pgbackrest.annotation"], "lucy")
					})
				}
			}
		}
	})

	t.Run("PGBouncer", func(t *testing.T) {
		cluster := testCluster()
		cluster.ObjectMeta.Name = "pgbouncer-cluster"
		cluster.ObjectMeta.Namespace = ns.Name
		cluster.Spec.Proxy.PGBouncer.Metadata = &v1beta1.Metadata{
			Annotations: map[string]string{"my.pgbouncer.annotation": "lucy"},
		}
		reconcileTestCluster(cluster)

		selector, err := naming.AsSelector(metav1.LabelSelector{
			MatchLabels: map[string]string{
				naming.LabelCluster: cluster.Name,
				naming.LabelRole:    naming.RolePGBouncer,
			},
		})
		assert.NilError(t, err)

		for _, gvk := range gvks {
			uList := &unstructured.UnstructuredList{}
			uList.SetGroupVersionKind(gvk)
			assert.NilError(t, reconciler.Client.List(ctx, uList,
				client.InNamespace(cluster.Namespace),
				client.MatchingLabelsSelector{Selector: selector}))

			for i := range uList.Items {
				u := uList.Items[i]

				annotations, err := getUnstructuredAnnotations(*cluster, u)
				assert.NilError(t, err)
				for resourceType, resourceAnnotations := range annotations {
					t.Run(u.GetKind()+"/"+u.GetName()+"/"+resourceType, func(t *testing.T) {
						assert.Equal(t, resourceAnnotations["my.pgbouncer.annotation"], "lucy")
					})
				}
			}
		}
	})
}

func TestContainerSecurityContext(t *testing.T) {
	if !strings.EqualFold(os.Getenv("USE_EXISTING_CLUSTER"), "true") {
		t.Skip("Test requires pods to be created")
	}

	t.Parallel()

	env, cc, config := setupTestEnv(t, ControllerName)
	t.Cleanup(func() { teardownTestEnv(t, env) })

	reconciler := &Reconciler{}
	ctx, cancel := setupManager(t, config, func(mgr manager.Manager) {
		reconciler = &Reconciler{
			Client:   cc,
			Owner:    client.FieldOwner(t.Name()),
			Recorder: mgr.GetEventRecorderFor(ControllerName),
			Tracer:   otel.Tracer(t.Name()),
		}
		podExec, err := newPodExecutor(config)
		assert.NilError(t, err)
		reconciler.PodExec = podExec
	})
	t.Cleanup(func() { teardownManager(cancel, t) })

	ns := &v1.Namespace{}
	ns.GenerateName = "postgres-operator-test-"
	ns.Labels = labels.Set{"postgres-operator-test": ""}
	assert.NilError(t, cc.Create(ctx, ns))
	t.Cleanup(func() { assert.Check(t, cc.Delete(ctx, ns)) })

	cluster := testCluster()
	cluster.Namespace = ns.Name

	assert.NilError(t, errors.WithStack(reconciler.Client.Create(ctx, cluster)))
	t.Cleanup(func() {
		// Remove finalizers, if any, so the namespace can terminate.
		assert.Check(t, client.IgnoreNotFound(
			reconciler.Client.Patch(ctx, cluster, client.RawPatch(
				client.Merge.Type(), []byte(`{"metadata":{"finalizers":[]}}`)))))
	})

	pods := &corev1.PodList{}
	assert.NilError(t, wait.Poll(time.Second, time.Second*120, func() (done bool, err error) {
		// Reconcile the cluster
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(cluster),
		})
		if err != nil {
			return false, err
		}
		if result.Requeue {
			return false, nil
		}

		err = reconciler.Client.List(ctx, pods,
			client.InNamespace(ns.Name),
			client.MatchingLabels{
				naming.LabelCluster: cluster.Name,
			})
		if err != nil {
			return false, err
		}

		// Can expect 4 pods from a cluster
		// instance, repo-host, pgbouncer, backup(s)
		if len(pods.Items) < 4 {
			return false, nil
		}
		return true, nil
	}))

	// Once we have a pod list with pods of each type, check that the
	// pods containers have the expected Security Context options
	for _, pod := range pods.Items {
		for _, container := range pod.Spec.Containers {
			assert.Equal(t, *container.SecurityContext.Privileged, false)
			assert.Equal(t, *container.SecurityContext.ReadOnlyRootFilesystem, true)
			assert.Equal(t, *container.SecurityContext.AllowPrivilegeEscalation, false)
		}
		for _, initContainer := range pod.Spec.InitContainers {
			assert.Equal(t, *initContainer.SecurityContext.Privileged, false)
			assert.Equal(t, *initContainer.SecurityContext.ReadOnlyRootFilesystem, true)
			assert.Equal(t, *initContainer.SecurityContext.AllowPrivilegeEscalation, false)
		}
	}
}

func TestGenerateClusterReplicaServiceIntent(t *testing.T) {
	env, cc, _ := setupTestEnv(t, ControllerName)
	t.Cleanup(func() { teardownTestEnv(t, env) })

	reconciler := &Reconciler{Client: cc}

	cluster := &v1beta1.PostgresCluster{}
	cluster.Namespace = "ns1"
	cluster.Name = "pg2"
	cluster.Spec.Port = initialize.Int32(9876)

	service, err := reconciler.generateClusterReplicaServiceIntent(cluster)
	assert.NilError(t, err)

	assert.Assert(t, marshalMatches(service.TypeMeta, `
apiVersion: v1
kind: Service
	`))
	assert.Assert(t, marshalMatches(service.ObjectMeta, `
creationTimestamp: null
labels:
  postgres-operator.crunchydata.com/cluster: pg2
  postgres-operator.crunchydata.com/role: replica
name: pg2-replicas
namespace: ns1
ownerReferences:
- apiVersion: postgres-operator.crunchydata.com/v1beta1
  blockOwnerDeletion: true
  controller: true
  kind: PostgresCluster
  name: pg2
  uid: ""
	`))
	assert.Assert(t, marshalMatches(service.Spec, `
ports:
- name: postgres
  port: 9876
  protocol: TCP
  targetPort: postgres
selector:
  postgres-operator.crunchydata.com/cluster: pg2
  postgres-operator.crunchydata.com/role: replica
type: ClusterIP
	`))

	t.Run("AnnotationsLabels", func(t *testing.T) {
		cluster := cluster
		cluster.Spec.Metadata = &v1beta1.Metadata{
			Annotations: map[string]string{"some": "note"},
			Labels:      map[string]string{"happy": "label"},
		}

		service, err := reconciler.generateClusterReplicaServiceIntent(cluster)
		assert.NilError(t, err)

		// Annotations present in the metadata.
		assert.Assert(t, marshalMatches(service.ObjectMeta.Annotations, `
some: note
		`))

		// Labels present in the metadata.
		assert.Assert(t, marshalMatches(service.ObjectMeta.Labels, `
happy: label
postgres-operator.crunchydata.com/cluster: pg2
postgres-operator.crunchydata.com/role: replica
		`))

		// Labels not in the selector.
		assert.Assert(t, marshalMatches(service.Spec.Selector, `
postgres-operator.crunchydata.com/cluster: pg2
postgres-operator.crunchydata.com/role: replica
		`))
	})
}
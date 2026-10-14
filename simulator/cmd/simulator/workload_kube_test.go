package main

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/util/flowcontrol"

	"github.com/matbun/joulie/simulator/pkg/workload"
)

// TestRunKubeResultsArriveAsEachCallFinishes: the run's Kubernetes calls
// share the client's rate limit, here 100 QPS as in a v1 run. A burst of
// 200 creates takes about 2 s; a completion patch queued behind it comes
// back within a few calls, and each create's result is there for the next
// tick once that create is done, not when the burst is. Results that wait
// for the whole burst stamp every submit and every completion late.
func TestRunKubeResultsArriveAsEachCallFinishes(t *testing.T) {
	limit := flowcontrol.NewTokenBucketRateLimiter(100, 1)
	fake := k8sfake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "done-pod"}})
	fake.PrependReactor("*", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		limit.Accept()
		return false, nil, nil
	})
	k := newRunKube(fake, "r1")
	k.listTwins = nil
	var cmds []kubeCmd
	for i := 0; i < 200; i++ {
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: fmt.Sprintf("job-%03d", i)}}
		cmds = append(cmds, kubeCmd{kind: cmdCreate, ns: "default", name: job.Name, obj: job})
	}
	cmds = append(cmds, kubeCmd{kind: cmdComplete, ns: "default", name: "done-pod", at: time.Now()})
	start := time.Now()
	k.push(cmds...)
	k.start()
	defer k.stop()

	var creates int
	var completedAfter time.Duration
	firstCreate := time.Duration(-1)
	for creates < 200 {
		if time.Since(start) > 20*time.Second {
			t.Fatalf("after 20 s: %d of 200 creates back", creates)
		}
		for _, res := range k.take().results {
			if res.err != nil {
				t.Fatalf("%v %s: %v", res.cmd.kind, res.cmd.name, res.err)
			}
			switch res.cmd.kind {
			case cmdCreate:
				creates++
				if firstCreate < 0 {
					firstCreate = time.Since(start)
				}
			case cmdComplete:
				completedAfter = time.Since(start)
				if creates > 50 {
					t.Errorf("the completion came back after %d of 200 creates, want it ahead of the burst", creates)
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	burst := time.Since(start)
	t.Logf("first create back after %v, completion after %v, all 200 creates after %v", firstCreate, completedAfter, burst)
	if completedAfter == 0 {
		t.Fatal("the completion never came back")
	}
	if firstCreate > burst/4 {
		t.Errorf("the first create came back after %v of a %v burst, want it as soon as it is done", firstCreate, burst)
	}
}

// runQueued runs every queued command on the calling goroutine, in the
// order the workers take them, so a test controls when results arrive.
func (k *runKube) runQueued(ctx context.Context) {
	for {
		c, ok := k.next()
		if !ok {
			return
		}
		k.keep(c, k.exec(ctx, c))
	}
}

// withDeleteCollection makes fake serve DeleteCollection of Jobs and pods,
// which its object tracker does not: it deletes the objects of the
// namespace that match the label selector, as the API server does, and
// records each call's delete options.
func withDeleteCollection(t *testing.T, fake *k8sfake.Clientset) *[]metav1.DeleteOptions {
	t.Helper()
	kinds := map[string]schema.GroupVersionKind{
		"jobs": batchv1.SchemeGroupVersion.WithKind("Job"),
		"pods": corev1.SchemeGroupVersion.WithKind("Pod"),
	}
	var mu sync.Mutex
	var calls []metav1.DeleteOptions
	fake.PrependReactor("delete-collection", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		dc := a.(k8stesting.DeleteCollectionActionImpl)
		mu.Lock()
		calls = append(calls, dc.DeleteOptions)
		mu.Unlock()
		gvr := dc.GetResource()
		gvk, ok := kinds[gvr.Resource]
		if !ok {
			return true, nil, fmt.Errorf("delete-collection of %s is not faked", gvr.Resource)
		}
		sel, err := labels.Parse(dc.ListOptions.LabelSelector)
		if err != nil {
			return true, nil, err
		}
		list, err := fake.Tracker().List(gvr, gvk, dc.GetNamespace())
		if err != nil {
			return true, nil, err
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			return true, nil, err
		}
		for _, item := range items {
			m, err := meta.Accessor(item)
			if err != nil {
				return true, nil, err
			}
			if sel.Matches(labels.Set(m.GetLabels())) {
				if err := fake.Tracker().Delete(gvr, m.GetNamespace(), m.GetName()); err != nil {
					return true, nil, err
				}
			}
		}
		return true, nil, nil
	})
	return &calls
}

// The cleanup of a finish deletes the run's objects with one
// DeleteCollection per namespace, by the run's label, and leaves other
// runs' objects alone: Job objects with propagation Background, so their
// pods go too, or the bare pods of an OwnerPod run. A role without the
// deletecollection verb gets them deleted one at a time.
func TestRunCleanupDeletesByCollection(t *testing.T) {
	label := workload.RunLabelValue("r1")
	obj := func(jobs bool, ns, name, run string) runtime.Object {
		om := metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{workload.LabelRunID: run}}
		if jobs {
			return &batchv1.Job{ObjectMeta: om}
		}
		return &corev1.Pod{ObjectMeta: om}
	}
	for _, tc := range []struct{ jobs, forbidden bool }{{true, false}, {false, false}, {true, true}, {false, true}} {
		jobs := tc.jobs
		var objs []runtime.Object
		for _, ns := range []string{"a", "b"} {
			for i := 0; i < 30; i++ {
				objs = append(objs, obj(jobs, ns, fmt.Sprintf("r1-%d", i), label))
			}
			objs = append(objs, obj(jobs, ns, "other", workload.RunLabelValue("r2")))
		}
		fake := k8sfake.NewSimpleClientset(objs...)
		calls := withDeleteCollection(t, fake)
		if tc.forbidden {
			fake.PrependReactor("delete-collection", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "", fmt.Errorf("no deletecollection"))
			})
		}
		cleanupObjects(context.Background(), newRunKube(fake, label), "r1", []string{"a", "b"}, jobs)

		var left []string
		if jobs {
			list, _ := fake.BatchV1().Jobs("").List(context.Background(), metav1.ListOptions{})
			for _, j := range list.Items {
				left = append(left, j.Namespace+"/"+j.Name)
			}
		} else {
			list, _ := fake.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{})
			for _, p := range list.Items {
				left = append(left, p.Namespace+"/"+p.Name)
			}
		}
		sort.Strings(left)
		if want := []string{"a/other", "b/other"}; !reflect.DeepEqual(left, want) {
			t.Errorf("jobs=%v: left %v, want only the other run's %v", jobs, left, want)
		}
		deletes := 0
		for _, a := range fake.Actions() {
			if a.GetVerb() == "delete" {
				deletes++
			}
		}
		if tc.forbidden {
			if deletes != 60 {
				t.Errorf("jobs=%v without deletecollection: %d single deletes, want 60", jobs, deletes)
			}
			continue
		}
		if len(*calls) != 2 || deletes != 0 {
			t.Errorf("jobs=%v: %d DeleteCollection calls and %d single deletes, want 2 and 0", jobs, len(*calls), deletes)
		}
		for _, o := range *calls {
			if o.PropagationPolicy == nil || *o.PropagationPolicy != metav1.DeletePropagationBackground {
				t.Errorf("jobs=%v: propagation %v, want Background", jobs, o.PropagationPolicy)
			}
		}
	}
}

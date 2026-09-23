//go:build envtest

package envtest_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/matbun/joulie/api/v1alpha1"
	joulie "github.com/matbun/joulie/pkg/api"
)

// The ownership contract, from the "Who writes what" table in the docs:
// the controller manager owns NodeTwin.spec and all of NodeTwin.status
// except status.controlStatus; the agent owns status.controlStatus and
// nothing else in the object.
//
// Unit tests check this by inspecting the shape of the patch payloads the
// components build, which proves what the code intends to write. Server-side
// apply proves what the API server records: each field is attributed to one
// manager in managedFields, and a manager reaching into the other's subtree
// is refused rather than silently merged.

// controllerManagerStatus is the shape the controller manager applies:
// everything under status except controlStatus.
func controllerManagerStatus(schedulableClass string, headroom float64) map[string]any {
	return map[string]any{
		"schedulableClass": schedulableClass,
		"powerMeasurement": map[string]any{
			"source":             joulie.PowerSourcePrometheus,
			"measuredNodePowerW": 310.5,
			"nodeTdpW":           500.0,
		},
		"predictedPowerHeadroomScore": headroom,
		"effectiveCapState": map[string]any{
			"cpuPct": 70.0,
			"gpuPct": 80.0,
		},
		"lastUpdated": "2026-09-23T10:00:00Z",
	}
}

// agentStatus is the shape the agent applies: status.controlStatus only.
func agentStatus(result string) map[string]any {
	return map[string]any{
		"controlStatus": map[string]any{
			"cpu": map[string]any{
				"backend":   "intel_rapl",
				"result":    result,
				"message":   "package cap set to 150 W",
				"updatedAt": "2026-09-23T10:00:05Z",
			},
		},
	}
}

// applySpec creates or updates the NodeTwin spec with server-side apply,
// as the controller manager does.
func applySpec(ctx context.Context, c client.Client, name, manager string, opts ...client.PatchOption) error {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "joulie.io/v1alpha1",
		"kind":       "NodeTwin",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"nodeName": name,
			"profile":  "eco",
			"cpu":      map[string]any{"packagePowerCapPctOfMax": 70.0},
		},
	}}
	return c.Patch(ctx, obj, client.Apply, append([]client.PatchOption{client.FieldOwner(manager)}, opts...)...)
}

// applyStatus applies a status subtree with the given field manager, the way
// each component writes its own part of NodeTwin.status.
func applyStatus(ctx context.Context, c client.Client, name, manager string, status map[string]any, opts ...client.SubResourcePatchOption) error {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "joulie.io/v1alpha1",
		"kind":       "NodeTwin",
		"metadata":   map[string]any{"name": name},
		"status":     status,
	}}
	return c.Status().Patch(ctx, obj, client.Apply, append([]client.SubResourcePatchOption{client.FieldOwner(manager)}, opts...)...)
}

func setUpSharedTwin(t *testing.T, c client.Client) string {
	t.Helper()
	ctx := testContext(t)
	name := objectName(t, "")
	if err := applySpec(ctx, c, name, joulie.FieldManagerControllerManager); err != nil {
		t.Fatalf("controller manager could not apply the spec: %v", err)
	}
	t.Cleanup(func() {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("NodeTwin"))
		obj.SetName(name)
		_ = c.Delete(testContext(t), obj)
	})
	return name
}

// TestBothManagersWriteTheirOwnSubtreeWithoutClobbering is the central
// ownership test: two field managers apply their own parts of the same
// status, repeatedly, and neither erases the other's fields. Server-side
// apply prunes the fields a manager owned and stopped sending, so a
// component whose payload accidentally covered the other's subtree would
// delete it here on its next write.
func TestBothManagersWriteTheirOwnSubtreeWithoutClobbering(t *testing.T) {
	c := newClient(t)
	ctx := testContext(t)
	name := setUpSharedTwin(t, c)

	if err := applyStatus(ctx, c, name, joulie.FieldManagerControllerManager, controllerManagerStatus("eco", 40)); err != nil {
		t.Fatalf("controller manager could not apply its status: %v", err)
	}
	if err := applyStatus(ctx, c, name, joulie.FieldManagerAgent, agentStatus("applied")); err != nil {
		t.Fatalf("agent could not apply status.controlStatus: %v", err)
	}

	got := getTwin(t, c, name)
	if got.Status.SchedulableClass != "eco" {
		t.Fatalf("agent write clobbered schedulableClass: %q", got.Status.SchedulableClass)
	}
	if got.Status.ControlStatus == nil || got.Status.ControlStatus.CPU == nil || got.Status.ControlStatus.CPU.Result != "applied" {
		t.Fatalf("controlStatus missing after the agent write: %+v", got.Status.ControlStatus)
	}

	// Second round: each manager writes again with new values. This is
	// where a payload that covers the other's subtree removes it.
	if err := applyStatus(ctx, c, name, joulie.FieldManagerControllerManager, controllerManagerStatus("performance", 88)); err != nil {
		t.Fatalf("controller manager second apply: %v", err)
	}
	got = getTwin(t, c, name)
	if got.Status.ControlStatus == nil || got.Status.ControlStatus.CPU == nil {
		t.Fatal("the controller manager's second status apply removed status.controlStatus, which the agent owns")
	}
	if got.Status.SchedulableClass != "performance" {
		t.Fatalf("controller manager could not update its own field: %q", got.Status.SchedulableClass)
	}

	if err := applyStatus(ctx, c, name, joulie.FieldManagerAgent, agentStatus("failed")); err != nil {
		t.Fatalf("agent second apply: %v", err)
	}
	got = getTwin(t, c, name)
	if got.Status.SchedulableClass != "performance" {
		t.Fatalf("the agent's second apply removed schedulableClass, which the controller manager owns: %q", got.Status.SchedulableClass)
	}
	if got.Status.PowerMeasurement == nil || got.Status.PredictedPowerHeadroomScore != 88 {
		t.Fatalf("the agent's second apply removed controller manager fields: %+v", got.Status)
	}
	if got.Status.ControlStatus.CPU.Result != "failed" {
		t.Fatalf("agent could not update its own field: %q", got.Status.ControlStatus.CPU.Result)
	}
	if got.Spec.Profile != "eco" || got.Spec.NodeName != name {
		t.Fatalf("the spec the controller manager owns did not survive the status writes: %+v", got.Spec)
	}
}

// TestManagedFieldsAttributeEachFieldToItsOwner reads back what the API
// server recorded. This is the machine-checkable version of the ownership
// table: a field that shows up under the wrong manager means a component is
// writing outside its subtree, whatever the payload shape suggested.
func TestManagedFieldsAttributeEachFieldToItsOwner(t *testing.T) {
	c := newClient(t)
	ctx := testContext(t)
	name := setUpSharedTwin(t, c)

	if err := applyStatus(ctx, c, name, joulie.FieldManagerControllerManager, controllerManagerStatus("eco", 40)); err != nil {
		t.Fatalf("controller manager status apply: %v", err)
	}
	if err := applyStatus(ctx, c, name, joulie.FieldManagerAgent, agentStatus("applied")); err != nil {
		t.Fatalf("agent status apply: %v", err)
	}

	got := getTwin(t, c, name)

	cmStatus := fieldsOf(t, got.ManagedFields, joulie.FieldManagerControllerManager, "status")
	for _, field := range []string{"f:schedulableClass", "f:powerMeasurement", "f:predictedPowerHeadroomScore", "f:effectiveCapState", "f:lastUpdated"} {
		if _, ok := cmStatus[field]; !ok {
			t.Fatalf("%s is not attributed to %s: %v", field, joulie.FieldManagerControllerManager, keysOf(cmStatus))
		}
	}
	if _, ok := cmStatus["f:controlStatus"]; ok {
		t.Fatalf("%s owns status.controlStatus, which belongs to the agent", joulie.FieldManagerControllerManager)
	}

	agentFields := fieldsOf(t, got.ManagedFields, joulie.FieldManagerAgent, "status")
	if _, ok := agentFields["f:controlStatus"]; !ok {
		t.Fatalf("status.controlStatus is not attributed to %s: %v", joulie.FieldManagerAgent, keysOf(agentFields))
	}
	if len(agentFields) != 1 {
		t.Fatalf("%s owns more than status.controlStatus: %v", joulie.FieldManagerAgent, keysOf(agentFields))
	}

	cmSpec := fieldsOf(t, got.ManagedFields, joulie.FieldManagerControllerManager, "")
	if _, ok := cmSpec["f:spec"]; !ok {
		t.Fatalf("spec is not attributed to %s: %v", joulie.FieldManagerControllerManager, keysOf(cmSpec))
	}
	for _, e := range got.ManagedFields {
		if e.Manager == joulie.FieldManagerAgent && e.Subresource == "" {
			t.Fatalf("%s owns fields outside the status subresource: %s", joulie.FieldManagerAgent, string(e.FieldsV1.Raw))
		}
	}
}

// TestTakingAFieldOwnedByTheOtherManagerConflicts proves the guard is real
// in both directions, and that force is the only way past it. Without this,
// "neither clobbers the other" could just mean neither happened to try.
func TestTakingAFieldOwnedByTheOtherManagerConflicts(t *testing.T) {
	c := newClient(t)
	ctx := testContext(t)
	name := setUpSharedTwin(t, c)

	if err := applyStatus(ctx, c, name, joulie.FieldManagerControllerManager, controllerManagerStatus("eco", 40)); err != nil {
		t.Fatalf("controller manager status apply: %v", err)
	}
	if err := applyStatus(ctx, c, name, joulie.FieldManagerAgent, agentStatus("applied")); err != nil {
		t.Fatalf("agent status apply: %v", err)
	}

	// The agent reaches into schedulableClass, which the controller manager owns.
	intruding := agentStatus("applied")
	intruding["schedulableClass"] = "draining"
	err := applyStatus(ctx, c, name, joulie.FieldManagerAgent, intruding)
	if err == nil {
		t.Fatal("the agent took status.schedulableClass from the controller manager without a conflict")
	}
	if !apierrors.IsConflict(err) {
		t.Fatalf("expected a field-manager conflict, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), joulie.FieldManagerControllerManager) {
		t.Fatalf("conflict does not name the owning manager: %v", err)
	}

	// The controller manager reaches into controlStatus, which the agent owns.
	intrudingCM := controllerManagerStatus("eco", 40)
	intrudingCM["controlStatus"] = map[string]any{
		"cpu": map[string]any{"backend": "intel_rapl", "result": "unsupported"},
	}
	err = applyStatus(ctx, c, name, joulie.FieldManagerControllerManager, intrudingCM)
	if err == nil {
		t.Fatal("the controller manager took status.controlStatus from the agent without a conflict")
	}
	if !apierrors.IsConflict(err) {
		t.Fatalf("expected a field-manager conflict, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), joulie.FieldManagerAgent) {
		t.Fatalf("conflict does not name the owning manager: %v", err)
	}

	// Force is the documented escape hatch, and it transfers ownership.
	if err := applyStatus(ctx, c, name, joulie.FieldManagerAgent, intruding, client.ForceOwnership); err != nil {
		t.Fatalf("forced apply was still refused: %v", err)
	}
	got := getTwin(t, c, name)
	if got.Status.SchedulableClass != "draining" {
		t.Fatalf("forced apply did not take effect: %q", got.Status.SchedulableClass)
	}
	agentFields := fieldsOf(t, got.ManagedFields, joulie.FieldManagerAgent, "status")
	if _, ok := agentFields["f:schedulableClass"]; !ok {
		t.Fatalf("force did not transfer ownership of schedulableClass: %v", keysOf(agentFields))
	}
}

func getTwin(t *testing.T, c client.Client, name string) *v1alpha1.NodeTwin {
	t.Helper()
	var twin v1alpha1.NodeTwin
	if err := c.Get(testContext(t), types.NamespacedName{Name: name}, &twin); err != nil {
		t.Fatalf("get NodeTwin %s: %v", name, err)
	}
	return &twin
}

// fieldsOf returns the fields one manager owns on one subresource. For the
// status subresource the leading "f:status" level is unwrapped, so the keys
// are the status fields themselves.
func fieldsOf(t *testing.T, entries []metav1.ManagedFieldsEntry, manager, subresource string) map[string]any {
	t.Helper()
	for _, e := range entries {
		if e.Manager != manager || e.Subresource != subresource || e.Operation != metav1.ManagedFieldsOperationApply {
			continue
		}
		if e.FieldsV1 == nil {
			t.Fatalf("managedFields entry for %s/%s has no fieldsV1", manager, subresource)
		}
		var fields map[string]any
		if err := json.Unmarshal(e.FieldsV1.Raw, &fields); err != nil {
			t.Fatalf("decode fieldsV1 for %s/%s: %v", manager, subresource, err)
		}
		if subresource != "status" {
			return fields
		}
		inner, ok := fields["f:status"].(map[string]any)
		if !ok {
			t.Fatalf("status entry for %s does not cover f:status: %s", manager, string(e.FieldsV1.Raw))
		}
		return inner
	}
	t.Fatalf("no apply entry in managedFields for manager %q subresource %q", manager, subresource)
	return nil
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

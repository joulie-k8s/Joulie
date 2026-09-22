// Package v1alpha1 holds the Go definitions of the joulie.io/v1alpha1 API.
// The CRD manifests in config/crd/bases and charts/joulie/crds are generated
// from these types by controller-gen (`make generate manifests`), so the
// schema has a single source of truth.
//
// The running components (agent, operator, scheduler) still exchange
// unstructured objects and the plain structs in pkg/api; these types exist
// for the CRD schema and for typed clients that may adopt them later.
//
// +kubebuilder:object:generate=true
// +groupName=joulie.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group and version of the objects in this package.
	GroupVersion = schema.GroupVersion{Group: "joulie.io", Version: "v1alpha1"}

	// SchemeBuilder registers the types of this package into a Scheme.
	// It uses apimachinery's builder so the package does not pull in
	// controller-runtime.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types of this package to a Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// Resource returns the GroupResource for a resource name of this group,
// for example Resource("nodetwins").
func Resource(resource string) schema.GroupResource {
	return GroupVersion.WithResource(resource).GroupResource()
}

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&NodeHardware{},
		&NodeHardwareList{},
		&NodeTwin{},
		&NodeTwinList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

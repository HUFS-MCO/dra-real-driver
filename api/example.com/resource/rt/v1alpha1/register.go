// +k8s:deepcopy-gen=package
// +groupName=rt.resource.example.com

package v1alpha1

import (
    "k8s.io/apimachinery/pkg/runtime"
    "k8s.io/apimachinery/pkg/runtime/schema"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
    // SchemeGroupVersion is group version used to register these objects
    SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: Version}

    // SchemeBuilder collects functions that add this API group to a scheme
    SchemeBuilder = runtime.NewSchemeBuilder(
        addKnownTypes,
    )

    // AddToScheme is the entry point to register all types with a scheme
    AddToScheme = SchemeBuilder.AddToScheme
)

// addKnownTypes registers your types with the given scheme
func addKnownTypes(scheme *runtime.Scheme) error {
    scheme.AddKnownTypes(SchemeGroupVersion,
        &DeviceClassParameters{},
        &ClaimParameters{},
    )
    metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
    return nil
}
package contracts_test

import (
	"go/build"
	"path/filepath"
	"strings"
	"testing"
)

// The policies are a contract with any scheduler: the node side
// (twin, policy, powerest) and the job side (the job description and the
// node state machine that reads it) import nothing from Kubernetes, in their
// code or their tests. A scheduler other than Kubernetes then needs only its
// own adapter beside pkg/job/kubejob (#121, #87).
func TestPoliciesImportNoKubernetes(t *testing.T) {
	root := repoRoot(t)
	for _, dir := range []string{
		"pkg/job",
		"pkg/controller/fsm",
		"pkg/controller/policy",
		"pkg/controller/twin",
		"pkg/scheduler/powerest",
	} {
		pkg, err := build.ImportDir(filepath.Join(root, dir), 0)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		imports := append(append(append([]string{}, pkg.Imports...), pkg.TestImports...), pkg.XTestImports...)
		for _, imp := range imports {
			if isKubernetesImport(imp) {
				t.Errorf("%s imports %s; read Kubernetes objects in an adapter (pkg/job/kubejob) and pass pkg/job values", dir, imp)
			}
		}
	}
}

func isKubernetesImport(path string) bool {
	for _, prefix := range []string{"k8s.io/", "sigs.k8s.io/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// The check above must see a Kubernetes import where there is one.
func TestPoliciesImportNoKubernetesSeesTheAdapter(t *testing.T) {
	pkg, err := build.ImportDir(filepath.Join(repoRoot(t), "pkg/job/kubejob"), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range pkg.Imports {
		if isKubernetesImport(imp) {
			return
		}
	}
	t.Fatalf("pkg/job/kubejob imports %v, no Kubernetes package: the import check reads nothing", pkg.Imports)
}

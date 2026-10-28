// conformance.go — the config tier's CR validity gate: every rendered
// overlay object applies cleanly to an envtest apiserver carrying the CRDs
// of a PINNED kernel chart release (the apiserver is the conformance
// surface — ADR-0011 amendment; no kernel conformance artifact exists).
//
// Usage (CI): go run ./hack/conformance -crds <dir> -objects <dir>...
//   -crds     directory of harmostes CRD yamls (extracted from the pinned chart)
//   -objects  one or more directories of rendered YAML (kustomize build out)
//
// Every document must be either empty or a harmostes.dev CR that the
// apiserver accepts. A document that fails (unknown field, bad enum,
// violated pattern) fails the run with the file + document index named —
// the !288 lesson (partial-line edits that glue YAML) must die here, not
// in a cluster.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	clientcmd "k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"gopkg.in/yaml.v3"
)

// docsOf splits a possibly multi-doc YAML file into individual documents.
// The parse gate (!288 lesson): a document that cannot round-trip a real
// parser fails BEFORE the apiserver ever sees it.
func docsOf(path string) ([][]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
	var docs [][]byte
	for {
		var node yaml.Node
		err := decoder.Decode(&node)
		if err != nil {
			break
		}
		if node.Kind == 0 {
			continue
		}
		out, err := yaml.Marshal(&node)
		if err != nil {
			return nil, fmt.Errorf("marshal doc: %w", err)
		}
		docs = append(docs, out)
	}
	return docs, nil
}

func main() {
	crdDir := flag.String("crds", "", "directory of CRD yamls")
	dirsList := flag.String("objects", "", "comma-separated directories of rendered objects")
	flag.Parse()
	dirs := strings.Split(*dirsList, ",")
	if *crdDir == "" {
		fmt.Fprintln(os.Stderr, "usage: conformance -crds <dir> -objects <dir>[,dir...]")
		os.Exit(2)
	}

	env := &envtest.Environment{CRDDirectoryPaths: []string{*crdDir}}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "conformance: envtest start: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = env.Stop() }()

	kcfg := &clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{"envtest": {
			Server:                   cfg.Host,
			CertificateAuthorityData: cfg.CAData,
			InsecureSkipTLSVerify:    false,
		}},
		Contexts:       map[string]*clientcmdapi.Context{"envtest": {Cluster: "envtest", AuthInfo: "envtest"}},
		CurrentContext: "envtest",
		AuthInfos:      map[string]*clientcmdapi.AuthInfo{"envtest": {ClientCertificateData: cfg.CertData, ClientKeyData: cfg.KeyData}},
	}
	restCfg, err := clientcmd.NewNonInteractiveClientConfig(*kcfg, "envtest", nil, nil).ClientConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "conformance: rest config: %v\n", err)
		os.Exit(1)
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "conformance: dynamic client: %v\n", err)
		os.Exit(1)
	}
	disco := discovery.NewDiscoveryClientForConfigOrDie(restCfg)
	resources, err := disco.ServerResourcesForGroupVersion("harmostes.dev/v1alpha1")
	if err != nil {
		fmt.Fprintf(os.Stderr, "conformance: harmostes.dev/v1alpha1 not served by the pinned CRDs: %v\n", err)
		os.Exit(1)
	}
	gvrFor := func(kind string) (schema.GroupVersionResource, error) {
		for _, r := range resources.APIResources {
			if r.Kind == kind {
				return schema.GroupVersionResource{Group: "harmostes.dev", Version: "v1alpha1", Resource: r.Name}, nil
			}
		}
		return schema.GroupVersionResource{}, fmt.Errorf("kind %q not served", kind)
	}

	files := []string{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "conformance: read %s: %v\n", dir, err)
			os.Exit(1)
		}
		for _, e := range entries {
			if e.Name() == "kustomization.yaml" {
				continue // build input, not an object (CI passes kustomize BUILT dirs; skip defensively anyway)
			}
			if strings.HasSuffix(e.Name(), ".yaml") || strings.HasSuffix(e.Name(), ".yml") {
				files = append(files, filepath.Join(dir, e.Name()))
			}
		}
	}
	sort.Strings(files)

	// envtest starts with no namespaces: pre-create every namespace a
	// rendered object targets (namespaced CRs cannot land without it).
	needNS := map[string]bool{}
	for _, f := range files {
		docs, err := docsOf(f)
		if err != nil {
			continue // the per-doc loop reports it
		}
		for _, doc := range docs {
			var obj map[string]any
			if err := yaml.Unmarshal(doc, &obj); err != nil || len(obj) == 0 {
				continue
			}
			u := &unstructured.Unstructured{Object: obj}
			if u.GetNamespace() != "" {
				needNS[u.GetNamespace()] = true
			}
		}
	}
	corev1 := dyn.Resource(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"})
	for ns := range needNS {
		if _, err := corev1.Create(context.Background(), &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": ns},
		}}, metav1.CreateOptions{FieldValidation: metav1.FieldValidationStrict}); err != nil {
			fmt.Fprintf(os.Stderr, "conformance: create namespace %s: %v\n", ns, err)
			os.Exit(1)
		}
	}

	applied, failures := 0, 0
	for _, f := range files {
		docs, err := docsOf(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL %s: %v\n", f, err)
			failures++
			continue
		}
		for i, doc := range docs {
			var obj map[string]any
			if err := yaml.Unmarshal(doc, &obj); err != nil {
				fmt.Fprintf(os.Stderr, "FAIL %s doc %d: parse: %v\n", f, i+1, err)
				failures++
				continue
			}
			if len(obj) == 0 {
				continue // empty separator document
			}
			u := &unstructured.Unstructured{Object: obj}
			gv, _ := schema.ParseGroupVersion(u.GetAPIVersion())
			if gv.Group != "harmostes.dev" {
				fmt.Fprintf(os.Stderr, "FAIL %s doc %d: not a harmostes.dev object (%s) — the config tier carries harmostes CRs only\n", f, i+1, u.GetAPIVersion())
				failures++
				continue
			}
			gvr, err := gvrFor(u.GetKind())
			if err != nil {
				fmt.Fprintf(os.Stderr, "FAIL %s doc %d: %v — is the pinned chart's CRD set current?\n", f, i+1, err)
				failures++
				continue
			}
			if _, err := dyn.Resource(gvr).Namespace(u.GetNamespace()).Create(context.Background(), u, metav1.CreateOptions{FieldValidation: metav1.FieldValidationStrict}); err != nil {
				body, _ := json.Marshal(u.Object)
				fmt.Fprintf(os.Stderr, "FAIL %s doc %d (%s %s): %v\n%s\n", f, i+1, u.GetKind(), u.GetName(), err, snippet(string(body)))
				failures++
				continue
			}
			applied++
		}
	}
	fmt.Printf("conformance: %d object(s) applied against the pinned CRDs, %d failure(s)\n", applied, failures)
	if failures > 0 {
		os.Exit(1)
	}
}

func snippet(s string) string {
	if len(s) > 600 {
		return s[:600] + "…"
	}
	return s
}

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"gopkg.in/yaml.v3"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/set"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"

	//
	// Uncomment to load all auth plugins
	// _ "k8s.io/client-go/plugin/pkg/client/auth"
	//
	// Or uncomment to load specific auth plugins
	// _ "k8s.io/client-go/plugin/pkg/client/auth/azure"
	// _ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	// _ "k8s.io/client-go/plugin/pkg/client/auth/oidc"

	appVersion "github.com/michaelvl/gateway-api-lens/pkg/version"

	client "sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
	gatewayv1b1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

const (
	dotGraphTemplateHeader string = `
digraph gatewayapi_config {
	rankdir = RL
	node [
		style="rounded,filled"
		shape=rect
		fillcolor=red
		penwidth=3
		pencolor="#00000044"
		fontname="Helvetica"
	]
	edge [
		arrowsize=0.5
		fontname="Helvetica"
		labeldistance=3
		labelfontcolor="#00000080"
		penwidth=2
		color="#888888"
		style=solid
	]
`
	dotGraphTemplateFooter string = `}
`
)

var (
	scheme = runtime.NewScheme()

	// Common shift in colourtable of graph node colours
	colourWheel = 8

	// Templates for nodes
	dotGatewayclassTemplate       = dotNodeTemplate("GatewayClass", false, colourWheel)
	dotGatewayclassparamsTemplate = dotNodeTemplate("%s", true, colourWheel+1)
	dotGatewayTemplate            = dotNodeTemplate("Gateway", false, colourWheel+2)
	dotHttprouteTemplate          = dotNodeTemplate("HTTPRoute", false, colourWheel+3)
	dotBackendTemplate            = dotNodeTemplate("%s", false, colourWheel+4)
	dotPolicyTemplate             = dotNodeTemplate("%s", true, colourWheel+5)
	dotEffpolicyTemplate          = dotNodeTemplate("Effective policy", true, colourWheel+7)

	// Header and footer for clusters of nodes
	dotClusterTemplateHeader = "\tsubgraph %s {\n\trankdir = TB\n\tcolor=none\n"
	dotClusterTemplateFooter = "\t}\n"

	// List of dotted-paths, e.g. 'spec.values' for values to show in graph output. Value must be of type map
	gwClassParameterPath *string

	// List of dotted-paths, e.g. 'spec.values.foo' which should be obfuscated. Useful for demos to avoid spilling intimate values
	paramObfuscateNumbersPaths ArgStringSlice
	paramObfuscateCharsPaths   ArgStringSlice

	kubeconfig                   *string
	outputFormat                 *string
	listenPort                   *int
	skipClustering               bool
	showPolicies                 bool
	showEffectivePolicies        bool
	useGatewayClassParamAsPolicy bool
	filterNamespaces             ArgStringSlice
	filterControllerName         ArgStringSlice

	// Clients
	cl  client.Client
	dcl *dynamic.DynamicClient
)

type State struct {
	gwcList          []GatewayClass
	gwList           []Gateway
	httpRtList       []HTTPRoute
	ingressList      *networkingv1.IngressList
	attachedPolicies []Policy
}

// This is the print-friendly format we process all resources into
type CommonObjRef struct {
	name      string
	namespace string
	group     string
	kind      string
	version   string

	// kind/name
	kindName string

	// 'Namespace/name' or 'Name' of target resource, depending on whether target is namespaced or cluster scoped
	namespacedName string

	// 'Kind/Namespace/name' or 'Kind/Name' of target resource, depending on whether target is namespaced or cluster scoped
	kindNamespacedName string

	// group/kind
	groupKind string

	// A predictable and unique ID from a combination of kind, namespace (if applicable) and resource name
	id string

	// Whether the object is namespaced
	isNamespaced bool

	// Marshalled YAML of body section (typically 'spec')
	bodyTxt  string
	bodyHTML string

	// Effective policy, i.e. merged according to precedence rules
	effPolicy *EffectivePolicy
}

// Processed information for GatewayClass resources
type GatewayClass struct {
	CommonObjRef

	controllerName string
	parameters     *GatewayClassParameters

	// The unprocessed resource
	raw *gatewayv1b1.GatewayClass

	// The unprocessed parameters
	rawParams *unstructured.Unstructured
}

// Processed information for GatewayClass resources
type GatewayClassParameters struct {
	CommonObjRef

	data map[string]any
}

// Processed information for Gateway resources
type Gateway struct {
	CommonObjRef

	class *GatewayClass

	// The unprocessed resource
	raw *gatewayv1b1.Gateway
}

// Processed information for HTTPRoute resources
type HTTPRoute struct {
	CommonObjRef

	backends []*CommonObjRef
	parents  []*Gateway

	// The unprocessed resource
	raw *gatewayv1b1.HTTPRoute
}

// Processed information for policy resources
type Policy struct {
	CommonObjRef

	targetRef *CommonObjRef

	spec map[string]any

	// Policy targetRef read from unstructured
	rawTargetRef *gatewayv1a2.PolicyTargetReference

	// The unprocessed resource
	raw *unstructured.Unstructured
}

type EffectivePolicy struct {
	// Policy data
	//nolint:unused // Currently unused, but possibly used in the future
	data map[string]any

	// Marshalled YAML of data
	bodyTxt  string
	bodyHTML string
}

type KubeObj interface {
	GetName() string
	GetNamespace() string
	GetObjectKind() schema.ObjectKind
}

type ResourceFilter struct {
	namespaces     []string
	controllerName []string
}

type Layout struct {
	showPolicies          bool
	showEffectivePolicies bool
}

type ArgStringSlice []string

func (p *ArgStringSlice) String() string {
	return strings.Join(*p, ";")
}
func (p *ArgStringSlice) Set(value string) error {
	*p = append(*p, value)
	return nil
}

func main() {
	log.Printf("version: %s\n", appVersion.Version)

	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1b1.AddToScheme(scheme))
	utilruntime.Must(apiextensions.AddToScheme(scheme))

	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	listenPort = flag.Int("l", 0, "port to run web server on and return graph output. Running server defaults to off")
	flag.Var(&filterNamespaces, "namespace", "Limit resources to namespace(s). Can be specified multiple times. Default is to use all namespaces")
	flag.Var(&filterControllerName, "controller-name", "Limit resources to those related to specific controller. Can be specified multiple times. Default is to use all controllers")
	outputFormat = flag.String("o", "policy", "output format [policy|graph|hierarchy|route-tree|gateways]")
	gwClassParameterPath = flag.String("gwc-param-path", "", "Dotted-path spec for data from GatewayClass parameters to show in graph output. Must be of type map")
	flag.BoolVar(&useGatewayClassParamAsPolicy, "use-gatewaylass-param-as-policy", true, "Use GatewayClass parameters as if they where a policy when calculating effective policy. Uses path given by `gwc-param-path` as policy `spec`.")

	flag.Var(&paramObfuscateNumbersPaths, "obfuscate-numbers", "Dotted-path spec for values from GatewayClass parameters and attached policies where numbers should be obfuscated.")
	flag.Var(&paramObfuscateCharsPaths, "obfuscate-chars", "Dotted-path spec for values from GatewayClass parameters and attached policies where characters should be obfuscated.")
	flag.BoolVar(&skipClustering, "skip-clustering", false, "Skip clustering in graph output.")
	flag.BoolVar(&showPolicies, "show-policies", false, "Show policies.")
	flag.BoolVar(&showEffectivePolicies, "show-effective-policies", false, "Show combined policy for Gateway resources.")

	flag.Parse()

	if skipClustering {
		dotClusterTemplateHeader = "\t# %s\n"
		dotClusterTemplateFooter = "\n"
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		if envvar := os.Getenv("KUBECONFIG"); len(envvar) > 0 {
			kubeconfig = &envvar
		}
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
		if err != nil {
			panic(err.Error())
		}
	}

	cl, err = client.New(config, client.Options{
		Scheme: scheme,
	})
	if err != nil {
		panic(err)
	}
	dcl = dynamic.NewForConfigOrDie(config)

	filters := ResourceFilter{filterNamespaces, filterControllerName}
	state, err := collectResources(cl, dcl, filters)
	if err != nil {
		panic(err)
	}

	if *listenPort != 0 {
		http.HandleFunc("/", httpHandler)
		server := &http.Server{
			Addr:              fmt.Sprintf(":%d", *listenPort),
			ReadHeaderTimeout: 3 * time.Second,
		}
		err := server.ListenAndServe()
		if err != nil {
			panic(err)
		}
	} else {
		switch *outputFormat {
		case "policy":
			outputTxtTablePolicyFocus(state)
		case "graph":
			layout := Layout{showPolicies, showEffectivePolicies}
			outputDotGraph(os.Stdout, state, layout)
		case "hierarchy":
			outputTxtClassHierarchy(state)
		case "route-tree":
			outputTxtRouteTree(state)
		case "gateways":
			outputTxtTableGatewayFocus(os.Stdout, state)
		}
	}
}

func httpHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filters := ResourceFilter{filterNamespaces, filterControllerName}
	if n, found := q["namespace"]; found {
		filters.namespaces = n
	}
	if c, found := q["controller-name"]; found {
		filters.controllerName = c
	}
	layout := Layout{}
	_, layout.showPolicies = q["show-policies"]
	_, layout.showEffectivePolicies = q["show-effective-policies"]

	state, err := collectResources(cl, dcl, filters)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	outputDotGraph(&buf, state, layout)

	fmt.Fprintf(w, `
<html><body>
<script type="module">
import { Graphviz } from "https://cdn.jsdelivr.net/npm/@hpcc-js/wasm/dist/index.js";

const graphviz = await Graphviz.load();
`)
	fmt.Fprintf(w, "const dot = `%s`;\n", buf.String())
	fmt.Fprintf(w, `
const svg = graphviz.dot(dot);
document.getElementById("gwapi").innerHTML = svg;
</script>
<div id="gwapi"></div>
</body></head>`)
}

//nolint:gocyclo // this function has a repeating character and thus not as complex as the number of loops indicate
func collectResources(cl client.Client, dcl *dynamic.DynamicClient, filters ResourceFilter) (*State, error) {
	var err error
	gwcList := &gatewayv1b1.GatewayClassList{}
	err = cl.List(context.TODO(), gwcList, client.InNamespace(""))
	if err != nil {
		return nil, fmt.Errorf("cannot list GatewayClasses: %w", err)
	}

	gwList := &gatewayv1b1.GatewayList{}
	err = cl.List(context.TODO(), gwList, client.InNamespace(""))
	if err != nil {
		return nil, fmt.Errorf("cannot list Gateways: %w", err)
	}

	httpRtList := &gatewayv1b1.HTTPRouteList{}
	err = cl.List(context.TODO(), httpRtList, client.InNamespace(""))
	if err != nil {
		return nil, fmt.Errorf("cannot list HTTPRoutes: %w", err)
	}

	ingressList := &networkingv1.IngressList{}
	err = cl.List(context.TODO(), ingressList, client.InNamespace(""))
	if err != nil {
		return nil, fmt.Errorf("cannot list Ingress: %w", err)
	}

	// Attached Policies, see https://gateway-api.sigs.k8s.io/geps/gep-713
	attachedPolicies := []unstructured.Unstructured{}
	crdResource := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	// This label identifies policy resources: https://gateway-api.sigs.k8s.io/geps/gep-713/#kubectl-plugin
	labSel, _ := labels.Parse("gateway.networking.k8s.io/policy=true")
	crdDefList, err := dcl.Resource(crdResource).List(context.TODO(), metav1.ListOptions{LabelSelector: labSel.String()})
	if err != nil {
		return nil, fmt.Errorf("cannot list CRDs: %w", err)
	}
	for idx := range crdDefList.Items {
		gvr, _ := crd2gvr(cl, &crdDefList.Items[idx])
		var crdList *unstructured.UnstructuredList
		crdList, err = dcl.Resource(*gvr).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("cannot list CRD %v: %w", gvr, err)
		}
		for _, crdInst := range crdList.Items {
			// We only queue CRDs with a 'targetRef'. TODO: Check that CRD also have 'kind' etc. fields.
			var found bool
			_, found, err = unstructured.NestedMap(crdInst.Object, "spec", "targetRef")
			if err != nil || !found {
				log.Printf("Missing or invalid targetRef, not a valid attached policy: %s/%s", crdInst.GetNamespace(), crdInst.GetName())
			} else {
				attachedPolicies = append(attachedPolicies, crdInst)
			}
		}
	}

	// svcList := &corev1.ServiceList{}
	// err = cl.List(context.TODO(), svcList, client.InNamespace(""))

	state := State{nil, nil, nil, ingressList, nil}

	//
	// Pre-process resources - common processing to make output
	// functions simpler by e.g. pre-formatting strings
	//

	// Pre-process policy resource
	for idx := range attachedPolicies {
		var isNamespaced, targetIsNamespaced bool
		policy := &attachedPolicies[idx]
		pol := Policy{}
		pol.raw = policy.DeepCopy()
		_, isNamespaced, err = unstructured2gvr(cl, policy)
		if err != nil {
			return nil, fmt.Errorf("cannot lookup GVR of %+v: %w", policy, err)
		}
		commonPreProc(&pol.CommonObjRef, policy, isNamespaced)
		pol.rawTargetRef, err = unstructured2TargetRef(policy)
		if err != nil {
			return nil, fmt.Errorf("cannot convert policy to targetRef: %w", err)
		}
		targetIsNamespaced, err = isNamespacedTargetRef(cl, pol.rawTargetRef)
		if err != nil {
			return nil, fmt.Errorf("cannot lookup namespace scope for targetRef %+v, policy %+v: %w", pol.rawTargetRef, policy, err)
		}
		objref := &CommonObjRef{}
		objref.name = string(pol.rawTargetRef.Name)
		objref.kind = string(pol.rawTargetRef.Kind)
		objref.group = string(pol.rawTargetRef.Group)
		if targetIsNamespaced {
			objref.isNamespaced = true
			objref.namespace = string(*pol.rawTargetRef.Namespace)
		} else {
			objref.isNamespaced = false
		}
		commonObjRefPreProc(objref)
		pol.targetRef = objref
		var spec map[string]any
		var found bool
		spec, found, err = unstructured.NestedMap(policy.Object, "spec")
		if err != nil {
			return nil, fmt.Errorf("cannot lookup policy spec: %w", err)
		}
		if found {
			// Delete 'targetRef' from spec before rendering BodyText, i.e. it will hold default/override only
			delete(spec, "targetRef")
			bodyTxt, bodyHTML := commonBodyTextProc(spec)
			c := &pol.CommonObjRef
			c.bodyTxt = bodyTxt
			c.bodyHTML += bodyHTML
			pol.spec = spec
		}
		state.attachedPolicies = append(state.attachedPolicies, pol)
	}

	// Pre-process GatewayClass resources
	for idx := range gwcList.Items {
		gwc := &gwcList.Items[idx]
		gwclass := GatewayClass{}
		commonPreProc(&gwclass.CommonObjRef, gwc, false)
		gwclass.raw = gwc.DeepCopy()
		gwclass.controllerName = string(gwc.Spec.ControllerName)
		if gwc.Spec.ParametersRef != nil {
			params := &GatewayClassParameters{}
			params.name = gwc.Spec.ParametersRef.Name
			params.kind = string(gwc.Spec.ParametersRef.Kind)
			params.group = string(gwc.Spec.ParametersRef.Group)
			if gwc.Spec.ParametersRef.Namespace != nil {
				params.isNamespaced = true
				params.namespace = string(*gwc.Spec.ParametersRef.Namespace)
			} else {
				params.isNamespaced = false
			}
			commonObjRefPreProc(&params.CommonObjRef)
			gwclass.parameters = params
			gwclass.rawParams, err = getGatewayParameters(cl, dcl, &gwclass)
			if err != nil {
				log.Printf("Cannot lookup GatewayClass parameter: %s\n", params.kindNamespacedName)
			} else if gwClassParameterPath != nil {
				pl := strings.Split(*gwClassParameterPath, ".")
				data, found, err := unstructured.NestedMap(gwclass.rawParams.Object, pl...)
				if err != nil {
					return nil, fmt.Errorf("cannot lookup GatewayClass parameter body: %w", err)
				}
				if found {
					params.bodyTxt, params.bodyHTML = commonBodyTextProc(data)
					params.data = data
				}
			}
		}
		state.gwcList = append(state.gwcList, gwclass)
	}

	// Pre-process Gateway resources
	for idx := range gwList.Items {
		gw := &gwList.Items[idx]
		g := Gateway{}
		commonPreProc(&g.CommonObjRef, gw, true)
		g.raw = gw.DeepCopy()
		g.class = state.gatewayclassByName(string(gw.Spec.GatewayClassName))
		calculateEffectivePolicy(&g, state.attachedPolicies)
		state.gwList = append(state.gwList, g)
	}

	// Pre-process HTTPRoute resources
	for idx := range httpRtList.Items {
		rt := &httpRtList.Items[idx]
		r := HTTPRoute{}
		commonPreProc(&r.CommonObjRef, rt, true)
		for _, rules := range rt.Spec.Rules {
			for _, backend := range rules.BackendRefs {
				objref := &CommonObjRef{}
				objref.name = string(backend.Name)
				objref.kind = string(Deref(backend.Kind, "Service"))
				objref.group = string(Deref(backend.Group, ""))
				isNamespaced, err := isNamespacedBackendObjectRef(cl, &backend.BackendObjectReference)
				if err != nil {
					return nil, fmt.Errorf("cannot detect if Backend resource is namespaced: %w", err)
				}
				if isNamespaced {
					objref.isNamespaced = true
					objref.namespace = string(Deref(backend.Namespace, gatewayv1b1.Namespace(rt.ObjectMeta.Namespace)))
				} else {
					objref.isNamespaced = false
				}
				commonObjRefPreProc(objref)
				r.backends = append(r.backends, objref)
			}
		}
		for _, parent := range rt.Spec.ParentRefs {
			if string(*parent.Kind) == "Gateway" {
				if gw := state.gatewayByName(string(parent.Name), string(Deref(parent.Namespace, gatewayv1b1.Namespace(rt.ObjectMeta.Namespace)))); gw != nil {
					r.parents = append(r.parents, gw)
				}
			}
		}
		r.raw = rt.DeepCopy()
		state.httpRtList = append(state.httpRtList, r)
	}

	// Apply filters - we deliberately do this on processed items to have access to processed fields
	if len(filters.controllerName) > 0 {
		cnames := set.New(filters.controllerName...)
		var newGwcList []GatewayClass
		for idx := range state.gwcList {
			if cnames.Has(state.gwcList[idx].controllerName) {
				newGwcList = append(newGwcList, state.gwcList[idx])
			}
		}
		state.gwcList = newGwcList
		var newGwList []Gateway
		for idx := range state.gwList {
			if cnames.Has(state.gwList[idx].class.controllerName) {
				newGwList = append(newGwList, state.gwList[idx])
			}
		}
		state.gwList = newGwList
		var newHTTPRtList []HTTPRoute
		for rtIdx := range state.httpRtList {
			for gwIdx := range state.httpRtList[rtIdx].parents {
				if cnames.Has(state.httpRtList[rtIdx].parents[gwIdx].class.controllerName) {
					newHTTPRtList = append(newHTTPRtList, state.httpRtList[rtIdx])
					break
				}
			}
		}
		state.httpRtList = newHTTPRtList
	}
	if len(filters.namespaces) > 0 {
		cnames := set.New(filters.namespaces...)
		var newGwList []Gateway
		for idx := range state.gwList {
			if cnames.Has(state.gwList[idx].namespace) {
				newGwList = append(newGwList, state.gwList[idx])
			}
		}
		state.gwList = newGwList
		var newhttpRtList []HTTPRoute
		for idx := range state.httpRtList {
			if cnames.Has(state.httpRtList[idx].namespace) {
				newhttpRtList = append(newhttpRtList, state.httpRtList[idx])
			}
		}
		state.httpRtList = newhttpRtList
	}

	return &state, nil
}

// Fill-in CommonObjRef fields from Kubernetes object
func commonPreProc(c *CommonObjRef, obj KubeObj, isNamespaced bool) {
	c.name = obj.GetName()
	c.isNamespaced = isNamespaced
	if isNamespaced {
		c.namespace = obj.GetNamespace()
	}
	objkind := obj.GetObjectKind()
	gvk := objkind.GroupVersionKind()
	c.kind = gvk.Kind
	c.group = gvk.Group
	c.version = gvk.Version
	commonObjRefPreProc(c)
}

// Calculate common strings from basic settings
func commonObjRefPreProc(c *CommonObjRef) {
	c.kindName = fmt.Sprintf("%s/%s", c.kind, c.name)
	if c.group == "" { // 'core' group
		c.groupKind = c.kind
	} else {
		c.groupKind = fmt.Sprintf("%s/%s", c.group, c.kind)
	}
	if c.isNamespaced {
		c.namespacedName = fmt.Sprintf("%s/%s", c.namespace, c.name)
		c.id = strings.ReplaceAll(fmt.Sprintf("%s_%s_%s", c.kind, c.namespace, c.name), "-", "_")
	} else {
		c.namespacedName = c.name
		c.id = strings.ReplaceAll(fmt.Sprintf("%s_%s", c.kind, c.name), "-", "_")
	}
	c.kindNamespacedName = fmt.Sprintf("%s/%s", c.kind, c.namespacedName)
}

func (s *State) gatewayclassByName(name string) *GatewayClass {
	for idx := range s.gwcList {
		gwc := &s.gwcList[idx]
		if gwc.name == name {
			return gwc
		}
	}
	return nil
}

func (s *State) gatewayByName(name, namespace string) *Gateway {
	for idx := range s.gwList {
		gw := &s.gwList[idx]
		if gw.name == name && gw.namespace == namespace {
			return gw
		}
	}
	return nil
}

// Pretty-print `body` and append to `c.bodyHTML`
func commonBodyTextProc(body map[string]any) (bodyTxt, bodyHTML string) {
	// Obfuscate
	for _, path := range paramObfuscateNumbersPaths {
		pl := strings.Split(path, ".")
		val, found, _ := unstructured.NestedString(body, pl...)
		if found {
			newstr := ""
			for _, r := range val {
				if r >= '0' && r <= '9' {
					r = 'x'
				}
				newstr += string(r)
			}
			err := unstructured.SetNestedField(body, newstr, pl...)
			if err != nil {
				log.Fatalf("Cannot obfuscate path %v: %v", path, err)
			}
		}
	}
	for _, path := range paramObfuscateCharsPaths {
		pl := strings.Split(path, ".")
		val, found, _ := unstructured.NestedString(body, pl...)
		if found {
			newstr := ""
			for _, r := range val {
				if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
					r = 'x'
				}
				newstr += string(r)
			}
			err := unstructured.SetNestedField(body, newstr, pl...)
			if err != nil {
				log.Fatalf("Cannot obfuscate path %v: %v", path, err)
			}
		}
	}
	// Pretty-print
	b, err := yaml.Marshal(body)
	if err != nil {
		log.Fatalf("Cannot marshal resource body selection: %v", err)
	}
	bodyTxt = string(b)
	bodyHTML = ""
	for _, ln := range strings.Split(bodyTxt, "\n") {
		if ln != "" { // To control leading white-space, replace ' ' by '<i> </i>'
			s := strings.TrimLeft(ln, " ")
			bodyHTML += fmt.Sprintf("%s%s<br/>", strings.Repeat("<i> </i>", len(ln)-len(s)), s)
		}
	}
	return bodyTxt, bodyHTML
}

// Deep map merge, with 'b' overwriting values in 'a'.  On type conflicts precedence is given to 'a' i.e. no overwrite
// x and y are concrete versions of a and b
func merge(a, b any) any {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			return a
		}
		for k, vy := range y { // Copy from 'b' (represented by 'y') into 'a'
			if va, ok := x[k]; ok {
				x[k] = merge(va, vy)
			} else {
				x[k] = vy
			}
		}
	default:
		return b
	}
	return a
}

func calculateEffectivePolicy(gw *Gateway, allPolicies []Policy) {
	var policies []Policy

	// FIXME: Consider to apply conflict resolution on policies

	selectPolicy := func(isAttachedTestFunc func(policy Policy, gw *Gateway) bool) {
		for idx := range allPolicies {
			p := &allPolicies[idx]
			if isAttachedTestFunc(*p, gw) {
				policies = append(policies, *p)
			}
		}
	}

	// Policies attached to GatewayClass of Gateway
	selectPolicy(func(policy Policy, gw *Gateway) bool {
		return policy.targetRef.kindNamespacedName == gw.class.kindNamespacedName
	})

	// Policies attached to Gateway namespace
	selectPolicy(func(policy Policy, gw *Gateway) bool {
		return policy.targetRef.kind == "Namespace" && policy.targetRef.name == gw.namespace
	})

	// Policies attached to Gateway
	selectPolicy(func(policy Policy, gw *Gateway) bool {
		return policy.targetRef.kindNamespacedName == gw.kindNamespacedName
	})

	p := EffectivePolicy{}
	data := map[string]any{}

	if useGatewayClassParamAsPolicy && gwClassParameterPath != nil && gw.class != nil && gw.class.parameters != nil && len(gw.class.parameters.data) > 0 {
		if def, found := gw.class.parameters.data["default"]; found {
			if d, ok := def.(map[string]any); ok {
				data = d
			}
		}
	}

	// overrides settings operate in a "less specific beats more specific" fashion
	for idx := range policies {
		if pdef, found := policies[idx].spec["default"]; found {
			if d, ok := merge(data, pdef).(map[string]any); ok {
				data = d
			} else {
				log.Printf("error calculating effective policy from %s\n", policies[idx].kindNamespacedName)
			}
		}
	}

	// defaults settings operate in a "more specific beats less specific" fashion
	for idx := len(policies) - 1; idx >= 0; idx-- {
		p := policies[idx]
		if povrd, found := p.spec["override"]; found {
			if d, ok := merge(data, povrd).(map[string]any); ok {
				data = d
			} else {
				log.Printf("error calculating effective policy from %s\n", p.kindNamespacedName)
			}
		}
	}

	p.bodyTxt, p.bodyHTML = commonBodyTextProc(data)
	gw.effPolicy = &p
}

//nolint:gocyclo // this function has a repeating character and thus not as complex as the number of loops indicate
func outputDotGraph(w io.Writer, s *State, layout Layout) {
	fmt.Fprint(w, dotGraphTemplateHeader)

	// Nodes, GatewayClasses and parameters
	fmt.Fprintf(w, dotClusterTemplateHeader, "cluster_gwc")
	for idx := range s.gwcList {
		gwc := &s.gwcList[idx]
		var params = fmt.Sprintf("Controller:<br/>%s", gwc.raw.Spec.ControllerName)
		fmt.Fprintf(w, dotGatewayclassTemplate, gwc.id, gwc.name, params)
		if layout.showEffectivePolicies && gwc.effPolicy != nil {
			fmt.Fprintf(w, dotEffpolicyTemplate, gwc.id+"_effpolicy", "", gwc.effPolicy.bodyHTML)
		}
		if gwc.parameters != nil {
			p := gwc.parameters
			fmt.Fprintf(w, dotGatewayclassparamsTemplate, p.id, p.groupKind, p.namespacedName, p.bodyHTML)
		}
	}
	fmt.Fprint(w, dotClusterTemplateFooter)

	// Nodes, Gateways
	fmt.Fprintf(w, dotClusterTemplateHeader, "cluster_gw")
	for idx := range s.gwList {
		var params string
		gw := &s.gwList[idx]
		for idx, l := range gw.raw.Spec.Listeners {
			if idx > 0 {
				params += "<br/>"
			}
			params += fmt.Sprintf("%s<br/>%s/%v", l.Name, l.Protocol, l.Port)
			if l.Hostname != nil {
				params += fmt.Sprintf("<br/><i>%s</i>", *l.Hostname)
			} else {
				params = "<br/><i>(no hostname)</i>"
			}
		}
		fmt.Fprintf(w, dotGatewayTemplate, gw.id, gw.namespacedName, params)
		if layout.showEffectivePolicies && gw.effPolicy != nil {
			fmt.Fprintf(w, dotEffpolicyTemplate, gw.id+"_effpolicy", "", gw.effPolicy.bodyHTML)
		}
	}
	fmt.Fprint(w, dotClusterTemplateFooter)

	// Nodes, HTTPRoutes
	fmt.Fprintf(w, dotClusterTemplateHeader, "cluster_httproute")
	for idx := range s.httpRtList {
		var params string
		rt := &s.httpRtList[idx]
		if rt.raw.Spec.Hostnames != nil {
			for idx, hname := range rt.raw.Spec.Hostnames {
				if idx > 0 {
					params += "<br/>"
				}
				params += string(hname)
			}
		} else {
			params = "<i>(no hostname)</i>"
		}
		fmt.Fprintf(w, dotHttprouteTemplate, rt.id, rt.namespacedName, params)
	}
	fmt.Fprint(w, dotClusterTemplateFooter)

	// Nodes, backends
	fmt.Fprintf(w, dotClusterTemplateHeader, "cluster_backends")
	for idx := range s.httpRtList {
		rt := &s.httpRtList[idx]
		for _, backend := range rt.backends {
			fmt.Fprintf(w, dotBackendTemplate, backend.id, backend.groupKind, backend.namespacedName, "? endpoint(s)")
		}
	}
	fmt.Fprint(w, dotClusterTemplateFooter)

	// Nodes, attached policies
	if layout.showPolicies {
		for idx := range s.attachedPolicies {
			policy := &s.attachedPolicies[idx]
			fmt.Fprintf(w, dotPolicyTemplate, policy.id, policy.groupKind, policy.namespacedName, policy.bodyHTML)
		}
	}

	// Edges
	for idx := range s.gwcList {
		gwc := &s.gwcList[idx]
		if gwc.parameters != nil {
			fmt.Fprintf(w, "\t%s -> %s\n", gwc.id, gwc.parameters.id)
		}
	}
	for idx := range s.gwList {
		gw := &s.gwList[idx]
		if gw.class != nil { // no matching gatewayclass
			fmt.Fprintf(w, "	%s -> %s\n", gw.id, gw.class.id)
			if layout.showEffectivePolicies {
				fmt.Fprintf(w, "	%s -> %s\n", gw.id+"_effpolicy", gw.id)
			}
		}
	}
	for idx := range s.httpRtList {
		rt := &s.httpRtList[idx]
		for _, pref := range rt.raw.Spec.ParentRefs {
			ns := rt.raw.ObjectMeta.Namespace
			if pref.Namespace != nil {
				ns = string(*pref.Namespace)
			}
			if pref.Kind != nil && *pref.Kind == gatewayv1b1.Kind("Gateway") {
				fmt.Fprintf(w, "	%s -> Gateway_%s_%s\n", rt.id, strings.ReplaceAll(ns, "-", "_"), strings.ReplaceAll(string(pref.Name), "-", "_"))
			}
		}
		for _, backend := range rt.backends {
			fmt.Fprintf(w, "	%s -> %s\n", backend.id, rt.id)
		}
	}
	// Edges, attached policies
	if layout.showPolicies {
		for idx := range s.attachedPolicies {
			policy := &s.attachedPolicies[idx]
			if policy.targetRef.kind == "Namespace" { // Namespace policies
				for idx := range s.gwList {
					gw := &s.gwList[idx]
					if gw.namespace == policy.targetRef.name && gw.class != nil { // no matching gatewayclass
						fmt.Fprintf(w, "	%s -> %s [style=dashed]\n", policy.id, gw.id)
					}
				}
			} else {
				fmt.Fprintf(w, "\t%s -> %s\n", policy.id, policy.targetRef.id)
			}
		}
	}
	fmt.Fprint(w, dotGraphTemplateFooter)
}

func outputTxtTablePolicyFocus(s *State) {
	t := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)
	fmt.Fprintln(t, "NAMESPACE\tPOLICY\tTARGET\tDEFAULT\tOVERRIDE")
	for idx := range s.attachedPolicies {
		policy := &s.attachedPolicies[idx]
		// Does the policy have 'default' or 'override' settings
		def := "No"
		override := "No"
		_, defFound, _ := unstructured.NestedMap(policy.raw.Object, "spec", "default")
		_, overrideFound, _ := unstructured.NestedMap(policy.raw.Object, "spec", "override")
		if defFound {
			def = "Yes"
		}
		if overrideFound {
			override = "Yes"
		}
		if policy.isNamespaced {
			fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\n", policy.namespace, policy.kindName, policy.targetRef.kindNamespacedName, def, override)
		} else {
			fmt.Fprintf(t, "\t%s\t%s\t%s\t%s\n", policy.kindName, policy.targetRef.kindNamespacedName, def, override)
		}
	}
	t.Flush()
}

// Hierarchy with GatewayClass at the top
func outputTxtClassHierarchy(s *State) {
	t := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)
	fmt.Fprintln(t, "RESOURCE\tCONFIGURATION")
	for idx := range s.gwcList {
		gwc := &s.gwcList[idx]
		fmt.Fprintf(t, "GatewayClass %s\tcontroller:%s\n", gwc.name, gwc.controllerName)
		for idx := range s.gwList {
			gw := &s.gwList[idx]
			if gw.class.id != gwc.id {
				continue
			}
			fmt.Fprintf(t, " ├─ Gateway %s\t", gw.namespacedName)
			for _, l := range gw.raw.Spec.Listeners {
				fmt.Fprintf(t, "%s:%s/%v ", l.Name, l.Protocol, l.Port)
				if l.Hostname != nil {
					fmt.Fprintf(t, "%s ", *l.Hostname)
				}
			}
			fmt.Fprintf(t, "\n")
			for idx := range s.httpRtList {
				rt := &s.httpRtList[idx]
				for _, pref := range rt.raw.Spec.ParentRefs {
					if IsRefToGateway(pref, gw) {
						fmt.Fprintf(t, "     ├─ HTTPRoute %s\t\n", rt.namespacedName)
						for _, rule := range rt.raw.Spec.Rules {
							fmt.Fprintf(t, "     │   ├─ match\t")
							for _, match := range rule.Matches {
								fmt.Fprintf(t, "%s %s ", *match.Path.Type, *match.Path.Value)
							}
							fmt.Fprintf(t, "\n")
							fmt.Fprintf(t, "     │   ├─ backends\t")
							for _, be := range rule.BackendRefs {
								fmt.Fprintf(t, "%s ", backendRef2String(&be.BackendRef))
							}
							fmt.Fprintf(t, "\n")
						}
						break // Only one parent ref for each gw
					}
				}
			}
		}
	}
	t.Flush()
}

// Routing trees
func outputTxtRouteTree(s *State) {
	t := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)
	fmt.Fprintln(t, "HOSTNAME/MATCH\tBACKEND")
	for idx := range s.gwList {
		gw := &s.gwList[idx]
		for _, l := range gw.raw.Spec.Listeners {
			if l.Hostname != nil {
				fmt.Fprintf(t, "%s\t", *l.Hostname)
			} else {
				fmt.Fprintf(t, "(none)\t")
			}
		}
		fmt.Fprintf(t, "\n")
		for idx := range s.httpRtList {
			rt := &s.httpRtList[idx]
			for _, pref := range rt.raw.Spec.ParentRefs {
				if IsRefToGateway(pref, gw) {
					for _, rule := range rt.raw.Spec.Rules {
						for _, match := range rule.Matches {
							fmt.Fprintf(t, "  ├─ %s %s\t", *match.Path.Type, *match.Path.Value)
							for _, be := range rule.BackendRefs {
								fmt.Fprintf(t, "%s ", backendRef2String(&be.BackendRef))
							}
						}
					}
					fmt.Fprintf(t, "\n")
					break // Only one parent ref for each gw
				}
			}
		}
	}
	t.Flush()
}

func outputTxtTableGatewayFocus(w io.Writer, s *State) {
	t := tabwriter.NewWriter(w, 0, 0, 1, ' ', 0)
	fmt.Fprintln(t, "GATEWAY\tCONFIGURATION")
	for idx := range s.gwList {
		gw := &s.gwList[idx]
		fmt.Fprintf(t, "%s\n", gw.namespacedName)
		if gw.effPolicy != nil {
			fmt.Fprintf(t, "%s\n", gw.effPolicy.bodyTxt)
		}
	}
	t.Flush()
}

func Deref[T any](ptr *T, deflt T) T {
	if ptr != nil {
		return *ptr
	}
	return deflt
}

func PtrTo[T any](val T) *T {
	return &val
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func unstructured2TargetRef(us *unstructured.Unstructured) (*gatewayv1a2.PolicyTargetReference, error) {
	targetRef, found, err := unstructured.NestedMap(us.Object, "spec", "targetRef")
	if !found || err != nil {
		return nil, err
	}
	group, found, err := unstructured.NestedString(targetRef, "group")
	if !found || err != nil {
		return nil, err
	}
	kind, found, err := unstructured.NestedString(targetRef, "kind")
	if !found || err != nil {
		return nil, err
	}
	name, found, err := unstructured.NestedString(targetRef, "name")
	if !found || err != nil {
		return nil, err
	}

	tRef := gatewayv1a2.PolicyTargetReference{Group: gatewayv1a2.Group(group), Kind: gatewayv1a2.Kind(kind), Name: gatewayv1a2.ObjectName(name)}

	namespace, found, _ := unstructured.NestedString(targetRef, "namespace")
	if !found {
		namespace = us.GetNamespace() // Defaults to namespace of policy
	}
	tRef.Namespace = PtrTo(gatewayv1a2.Namespace(namespace))

	return &tRef, nil
}

func IsRefToGateway(parentRef gatewayv1b1.ParentReference, gateway *Gateway) bool {
	if parentRef.Group != nil && string(*parentRef.Group) != gatewayv1b1.GroupName {
		return false
	}

	if parentRef.Kind != nil && string(*parentRef.Kind) != "Gateway" {
		return false
	}

	if parentRef.Namespace != nil && string(*parentRef.Namespace) != gateway.namespace {
		return false
	}

	return string(parentRef.Name) == gateway.name
}

func backendRef2String(be *gatewayv1b1.BackendRef) string {
	beo := be.BackendObjectReference
	out := string(beo.Name)
	if beo.Namespace != nil {
		out = fmt.Sprintf("%s/%s", *beo.Namespace, out)
	}
	if beo.Kind != nil {
		out = fmt.Sprintf("%s/%s", *beo.Kind, out)
	}
	if beo.Port != nil {
		out = fmt.Sprintf("%s:%v", out, *beo.Port)
	}
	if be.Weight != nil {
		out = fmt.Sprintf("%s@%v", out, *be.Weight)
	}
	return out
}

// Return true if targetref is to a namespaced resource
func isNamespacedTargetRef(cl client.Client, tRef *gatewayv1a2.PolicyTargetReference) (bool, error) {
	gk := schema.GroupKind{
		Group: string(tRef.Group),
		Kind:  string(tRef.Kind),
	}
	mapping, err := cl.RESTMapper().RESTMapping(gk)
	if err != nil {
		return false, err
	}
	return mapping.Scope.Name() == meta.RESTScopeNameNamespace, nil
}

// Return true if backendobjectref is to a namespaced resource
func isNamespacedBackendObjectRef(cl client.Client, bRef *gatewayv1b1.BackendObjectReference) (bool, error) {
	gk := schema.GroupKind{
		Group: string(Deref(bRef.Group, "")),
		Kind:  string(Deref(bRef.Kind, "Service")),
	}
	mapping, err := cl.RESTMapper().RESTMapping(gk)
	if err != nil {
		return false, err
	}
	return mapping.Scope.Name() == meta.RESTScopeNameNamespace, nil
}

// Lookup GVR for resource in unstructured.Unstructured
func unstructured2gvr(cl client.Client, us *unstructured.Unstructured) (*schema.GroupVersionResource, bool, error) {
	gv, err := schema.ParseGroupVersion(us.GetAPIVersion())
	if err != nil {
		return nil, false, err
	}
	gk := schema.GroupKind{
		Group: gv.Group,
		Kind:  us.GetKind(),
	}
	mapping, err := cl.RESTMapper().RESTMapping(gk, gv.Version)
	if err != nil {
		return nil, false, err
	}

	isNamespaced := mapping.Scope.Name() == meta.RESTScopeNameNamespace

	return &mapping.Resource, isNamespaced, nil
}

// Lookup GVR for CRD in unstructured.Unstructured
func crd2gvr(cl client.Client, crd *unstructured.Unstructured) (*schema.GroupVersionResource, error) {
	group, found, err := unstructured.NestedString(crd.Object, "spec", "group")
	if err != nil || !found {
		return nil, fmt.Errorf("cannot lookup group")
	}
	kind, found, err := unstructured.NestedString(crd.Object, "spec", "names", "kind")
	if err != nil || !found {
		return nil, fmt.Errorf("cannot lookup kind")
	}
	versions, found, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	if err != nil || !found {
		return nil, fmt.Errorf("cannot lookup kind")
	}
	versionsMap, ok := versions[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("cannot lookup CRD version[0]")
	}
	version, ok := versionsMap["name"].(string)
	if !ok {
		return nil, fmt.Errorf("cannot lookup CRD version[0]")
	}

	gk := schema.GroupKind{
		Group: group,
		Kind:  kind,
	}
	mapping, err := cl.RESTMapper().RESTMapping(gk, version)
	if err != nil {
		return nil, err
	}

	return &mapping.Resource, nil
}

func getGatewayParameters(cl client.Client, dcl *dynamic.DynamicClient, gwc *GatewayClass) (*unstructured.Unstructured, error) {
	var res *unstructured.Unstructured
	var err error

	gk := schema.GroupKind{
		Group: gwc.parameters.group,
		Kind:  gwc.parameters.kind,
	}
	mapping, err := cl.RESTMapper().RESTMapping(gk)
	if err != nil {
		return nil, err
	}

	isNamespaced := mapping.Scope.Name() == meta.RESTScopeNameNamespace
	gvr := mapping.Resource

	if isNamespaced {
		res, err = dcl.Resource(gvr).Namespace(gwc.parameters.namespace).Get(context.TODO(), gwc.parameters.name, metav1.GetOptions{})
	} else {
		res, err = dcl.Resource(gvr).Get(context.TODO(), gwc.parameters.name, metav1.GetOptions{})
	}
	if err != nil {
		log.Fatalf("Cannot get parameters %+v: %v", gvr, err)
		return nil, err
	}
	return res, nil
}

func dotNodeTemplate(resType string, leftAlignBody bool, colourWheel int) string {
	var colours = []string{"EF9A9A", "F48FB1", "CE93D8", "B39DDB", "9FA8DA", "90CAF9", "81D4FA",
		"80DEEA", "80CBC4", "A5D6A7", "C5E1A5", "E6EE9C", "FFF59D", "FFE082", "FFCC80",
		"FFAB91", "BCAAA4", "EEEEEE", "B0BEC5"}
	var r, g, b uint8
	shade := 0.8
	light := 1.4

	rand.Seed(9)
	rand.Shuffle(len(colours), func(i, j int) { colours[i], colours[j] = colours[j], colours[i] })
	colour := colours[colourWheel%len(colours)]
	fmt.Sscanf(colour, "%02x%02x%02x", &r, &g, &b)
	sr := uint8(float64(r) * shade)
	sg := uint8(float64(g) * shade)
	sb := uint8(float64(b) * shade)
	lr := min(int(float64(r)*light), 255)
	lg := min(int(float64(g)*light), 255)
	lb := min(int(float64(b)*light), 255)
	fillcolour := fmt.Sprintf("%02x%02x%02x", lr, lg, lb)
	edgecolour := fmt.Sprintf("%02x%02x%02x", sr, sg, sb)

	bodyAlign := ""
	if leftAlignBody {
		bodyAlign = ` align="left" balign="left"`
	}

	return `	%s [
		fillcolor="#` + fillcolour + `"
		color="#` + edgecolour + `"
		label=<<table border="0" cellborder="1" cellspacing="0" cellpadding="4">
			<tr> <td sides="B"> <b>` + resType + `</b><br/>%s</td> </tr>
			<tr> <td sides="T"` + bodyAlign + `>%s</td> </tr>
		</table>>
	]` + "\n"
}

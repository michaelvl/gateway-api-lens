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

	//"k8s.io/client-go/kubernetes"
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

	"github.com/michaelvl/gateway-api-lens/pkg/version"

	client "sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
	gatewayv1b1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

const (
	dot_graph_template_header string = `
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
	dot_graph_template_footer string = `}
`
)

var (
	scheme = runtime.NewScheme()

	// Common shift in colourtable of graph node colours
	colourWheel = 8
	dot_gatewayclass_template       = gv_node_template("GatewayClass", false, colourWheel)
	dot_gatewayclassparams_template = gv_node_template("%s", true, colourWheel+1)
	dot_gateway_template            = gv_node_template("Gateway", false, colourWheel+2)
	dot_httproute_template          = gv_node_template("HTTPRoute", false, colourWheel+3)
	dot_backend_template            = gv_node_template("%s", false, colourWheel+4)
	dot_policy_template             = gv_node_template("%s", true, colourWheel+5)
	dot_effpolicy_template          = gv_node_template("Effective policy", true, colourWheel+7)

	dot_cluster_template_header     = "\tsubgraph %s {\n\trankdir = TB\n\tcolor=none\n"
	dot_cluster_template_footer     = "\t}\n"

	// List of dotted-paths, e.g. 'spec.values' for values to show in graph output. Value must be of type map
	gwClassParameterPath *string

	// List of dotted-paths, e.g. 'spec.values.foo' which should be obfuscated. Useful for demos to avoid spilling intimate values
	paramObfuscateNumbersPaths ArgStringSlice
	paramObfuscateCharsPaths ArgStringSlice

	kubeconfig            *string
	outputFormat          *string
	listenPort            *string
	skipClustering        bool
	showPolicies          bool
	showEffectivePolicies bool
	useGatewayClassParamAsPolicy bool
	filterNamespaces      ArgStringSlice
	filterControllerName  ArgStringSlice

	cl                client.Client
	dcl               *dynamic.DynamicClient
)

type State struct {
	gwcList           []GatewayClass
	gwList            []Gateway
	httpRtList        []HTTPRoute
	ingressList       *networkingv1.IngressList
	attachedPolicies  []Policy
}

// This is the print-friendly format we process all resources into
type CommonObjRef struct {
	name           string
	namespace      string
	group          string
	kind           string
	version        string

	// kind/name
	kindName       string

	// 'Namespace/name' or 'Name' of target resource, depending on whether target is namespaced or cluster scoped
	namespacedName string

	// 'Kind/Namespace/name' or 'Kind/Name' of target resource, depending on whether target is namespaced or cluster scoped
	kindNamespacedName string

	// group/kind
	groupKind      string

	// A predictable and unique ID from a combination of kind, namespace (if applicable) and resource name
	id             string

	// Whether the object is namespaced
	isNamespaced   bool

	// Marshalled YAML of body section (typically 'spec')
	bodyTxt string
	bodyHtml string

	// Effective policy, i.e. merged according to precedence rules
	effPolicy      *EffectivePolicy
}

// Processed information for GatewayClass resources
type GatewayClass struct {
	CommonObjRef

	controllerName   string
	parameters       *GatewayClassParameters

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

	backends     []*CommonObjRef
	parents      []*Gateway

	// The unprocessed resource
	raw *gatewayv1b1.HTTPRoute
}

// Processed information for policy resources
type Policy struct {
	CommonObjRef

	targetRef    *CommonObjRef

	spec         map[string]any

	// Policy targetRef read from unstructured
	rawTargetRef *gatewayv1a2.PolicyTargetReference

	// The unprocessed resource
	raw *unstructured.Unstructured
}

type EffectivePolicy struct {
	// Policy data
	data map[string]any

	// Marshalled YAML of data
	bodyTxt string
	bodyHtml string
}

type KubeObj interface {
	GetName() string
	GetNamespace() string
	GetObjectKind() schema.ObjectKind
}

type ArgStringSlice []string
func (i *ArgStringSlice) String() string {
	return strings.Join(*i, ";")
}
func (p *ArgStringSlice) Set(value string) error {
	*p = append(*p, value)
	return nil
}

func main() {
	log.Printf("version: %s\n", version.Version)

	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1b1.AddToScheme(scheme))
	utilruntime.Must(apiextensions.AddToScheme(scheme))

	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	listenPort = flag.String("l", "", "port to run web server on and return graph output. Defaults to off")
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
		dot_cluster_template_header     = "\t# %s\n"
		dot_cluster_template_footer     = "\n"
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		if envvar := os.Getenv("KUBECONFIG"); len(envvar) >0 {
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
		panic(err.Error())
	}
	dcl = dynamic.NewForConfigOrDie(config)

	state, err := collectResources(cl, dcl)

	if *listenPort != "" {
		http.HandleFunc("/", httpHandler)
		log.Fatal(http.ListenAndServe(":"+string(*listenPort), nil))
	} else {
		switch *outputFormat {
		case "policy":
			outputTxtTablePolicyFocus(state)
		case "graph":
			outputDotGraph(os.Stdout, state)
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
	filterNamespaces = q["namespace"]
	filterControllerName = q["controller-name"]
	_, showEffectivePolicies = q["show-effective-policies"]

	state, err := collectResources(cl, dcl)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	outputDotGraph(&buf, state)

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

// FIXME: Improve error handling
func collectResources(cl client.Client, dcl *dynamic.DynamicClient) (*State, error) {
	var err error
	gwcList := &gatewayv1b1.GatewayClassList{}
	err = cl.List(context.TODO(), gwcList, client.InNamespace(""))

	gwList := &gatewayv1b1.GatewayList{}
	err = cl.List(context.TODO(), gwList, client.InNamespace(""))

	httpRtList := &gatewayv1b1.HTTPRouteList{}
	err = cl.List(context.TODO(), httpRtList, client.InNamespace(""))

	ingressList := &networkingv1.IngressList{}
	err = cl.List(context.TODO(), ingressList, client.InNamespace(""))

	// Attached Policies, see https://gateway-api.sigs.k8s.io/geps/gep-713
	attachedPolicies := []unstructured.Unstructured{}
	crdResource := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	// This label identifies policy resources: https://gateway-api.sigs.k8s.io/geps/gep-713/#kubectl-plugin
	labSel, _ := labels.Parse("gateway.networking.k8s.io/policy=true")
	crdDefList, err := dcl.Resource(crdResource).List(context.TODO(), metav1.ListOptions{LabelSelector: labSel.String()})
	if err != nil {
		log.Fatalf("Cannot list CRDs: %v", err)
	}
	for _, crd := range crdDefList.Items {
		gvr, _ := crd2gvr(cl, &crd)
		crdList, err := dcl.Resource(*gvr).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			log.Fatalf("Cannot list CRD %v: %v", gvr, err)
		}
		for _, crdInst := range crdList.Items {
			// We only queue CRDs with a 'targetRef'. TODO: Check that CRD also have 'kind' etc. fields.
			_, found, err := unstructured.NestedMap(crdInst.Object, "spec", "targetRef")
			if err != nil || !found {
				log.Printf("Missing or invalid targetRef, not a valid attached policy: %s/%s", crdInst.GetNamespace(), crdInst.GetName())
			} else {
				attachedPolicies = append(attachedPolicies, crdInst)
			}
		}
	}

	//svcList := &corev1.ServiceList{}
	//err = cl.List(context.TODO(), svcList, client.InNamespace(""))

	state := State{nil, nil, nil, ingressList, nil}

	//
	// Pre-process resources - common processing to make output
	// functions simpler by e.g. pre-formatting strings
	//

	// Pre-process policy resource
	for _, policy := range attachedPolicies {
		pol := Policy{}
		pol.raw = policy.DeepCopy()
		_, isNamespaced, err := unstructured2gvr(cl, &policy)
		if err != nil {
			log.Fatalf("Cannot lookup GVR of %+v: %w", policy, err)
		}
		commonPreProc(&pol.CommonObjRef, &policy, isNamespaced, nil)
		pol.rawTargetRef, err = unstructured2TargetRef(policy)
		if err != nil {
			log.Fatalf("Cannot convert policy to targetRef: %w", err)
		}
		targetIsNamespaced, err := isNamespacedTargetRef(cl, pol.rawTargetRef)
		if err != nil {
			log.Fatalf("Cannot lookup namespace scope for targetRef %+v, policy %+v", pol.rawTargetRef, policy)
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
		spec,found,err := unstructured.NestedMap(policy.Object, "spec")
		if err != nil {
			log.Fatalf("Cannot lookup policy spec: %w", err)
		}
		if found {
			// Delete 'targetRef' from spec before rendering BodyText, i.e. it will hold default/override only
			delete(spec, "targetRef")
			bodyTxt, bodyHtml := commonBodyTextProc(spec)
			c := &pol.CommonObjRef
			c.bodyTxt = bodyTxt
			c.bodyHtml += bodyHtml
			pol.spec = spec
		}
		state.attachedPolicies = append(state.attachedPolicies, pol)
	}

	// Pre-process GatewayClass resources
	for _, gwc := range gwcList.Items {
		gwclass := GatewayClass{}
		commonPreProc(&gwclass.CommonObjRef, &gwc, false, state.attachedPolicies)
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
				data,found,err := unstructured.NestedMap(gwclass.rawParams.Object, pl...)
				if err != nil {
					log.Fatalf("Cannot lookup GatewayClass parameter body: %w", err)
				}
				if found {
					params.bodyTxt, params.bodyHtml = commonBodyTextProc(data)
					params.data = data

				}
			}
		}
		state.gwcList = append(state.gwcList, gwclass)
	}

	// Pre-process Gateway resources
	for _, gw := range gwList.Items {
		g := Gateway{}
		commonPreProc(&g.CommonObjRef, &gw, true, state.attachedPolicies)
		g.raw = gw.DeepCopy()
		g.class = state.gatewayclassByName(string(gw.Spec.GatewayClassName))
		calculateEffectivePolicy(&g, state.attachedPolicies)
		state.gwList = append(state.gwList, g)
	}

	// Pre-process HTTPRoute resources
	for _, rt := range httpRtList.Items {
		r := HTTPRoute{}
		commonPreProc(&r.CommonObjRef, &rt, true, state.attachedPolicies)
		for _, rules := range rt.Spec.Rules {
			for _, backend := range rules.BackendRefs {
				objref := &CommonObjRef{}
				objref.name = string(backend.Name)
				objref.kind = string(Deref(backend.Kind, "Service"))
				objref.group = string(Deref(backend.Group, ""))
				isNamespaced, err := isNamespacedBackendObjectRef(cl, &backend.BackendObjectReference)
				if err != nil {
					log.Fatalf("Cannot detect if Backend resource is namespaced: %w", err)
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
				if gw := state.gatewayByName(string(parent.Name), string(*parent.Namespace)); gw != nil {
					r.parents = append(r.parents, gw)
				}
			}
		}
		r.raw = rt.DeepCopy()
		state.httpRtList = append(state.httpRtList, r)
	}

	// Apply filters - we deliberately do this on processed items to have access to processed fields
	if len(filterControllerName) > 0 {
		cnames := set.New(filterControllerName...)
		var newGwcList []GatewayClass
		for _, gwc := range state.gwcList {
			if cnames.Has(string(gwc.controllerName)) {
				newGwcList = append(newGwcList, gwc)
			}
		}
		state.gwcList = newGwcList
		var newGwList []Gateway
		for _, gw := range state.gwList {
			if cnames.Has(string(gw.class.controllerName)) {
				newGwList = append(newGwList, gw)
			}
		}
		state.gwList = newGwList
		var newHttpRtList []HTTPRoute
		for _, rt := range state.httpRtList {
			for _, gw := range rt.parents {
				if cnames.Has(string(gw.class.controllerName)) {
					newHttpRtList = append(newHttpRtList, rt)
					break
				}
			}
		}
		state.httpRtList = newHttpRtList
	}
	if len(filterNamespaces) > 0 {
		cnames := set.New(filterNamespaces...)
		var newGwList []Gateway
		for _, gw := range state.gwList {
			if cnames.Has(string(gw.namespace)) {
				newGwList = append(newGwList, gw)
			}
		}
		state.gwList = newGwList
		var newhttpRtList []HTTPRoute
		for _, rt := range state.httpRtList {
			if cnames.Has(string(rt.namespace)) {
				newhttpRtList = append(newhttpRtList, rt)
			}
		}
		state.httpRtList = newhttpRtList
	}

	return &state, nil
}

// Fill-in CommonObjRef fields from Kubernetes object
func commonPreProc(c *CommonObjRef, obj KubeObj, isNamespaced bool, policies []Policy) {
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
	for _, gwc := range s.gwcList {
		if gwc.name == name {
			return &gwc
		}
	}
	return nil
}

func (s *State) gatewayByName(name, namespace string) *Gateway {
	for _, gw := range s.gwList {
		if gw.name == name && gw.namespace == namespace {
			return &gw
		}
	}
	return nil
}

// Pretty-print `body` and append to `c.bodyHtml`
func commonBodyTextProc(body map[string]any) (string, string) {
	// Obfuscate
	for _,path := range paramObfuscateNumbersPaths {
		pl := strings.Split(path, ".")
		val,found,_ := unstructured.NestedString(body, pl...)
		if found {
			newstr := ""
			for _,r := range val {
				if (r >= '0' && r <= '9') {
					r = 'x'
				}
				newstr += string(r)
			}
			unstructured.SetNestedField(body, newstr, pl...)
		}
	}
	for _,path := range paramObfuscateCharsPaths {
		pl := strings.Split(path, ".")
		val,found,_ := unstructured.NestedString(body, pl...)
		if found {
			newstr := ""
			for _,r := range val {
				if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
					r = 'x'
				}
				newstr += string(r)
			}
			unstructured.SetNestedField(body, newstr, pl...)
		}
	}
	// Pretty-print
	b, err := yaml.Marshal(body)
	if err != nil {
		log.Fatalf("Cannot marshal resource body selection: %w", err)
	}
	bodyTxt := string(b)
	bodyHtml := ""
 	for _,ln := range strings.Split(bodyTxt, "\n") {
		if ln != "" { // To control leading white-space, replace ' ' by '<i> </i>'
			s := strings.TrimLeft(ln, " ")
			bodyHtml += fmt.Sprintf("%s%s<br/>", strings.Repeat("<i> </i>", len(ln)-len(s)), s)
		}
	}
	return bodyTxt, bodyHtml
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

	selectPolicy := func(isAttached func(policy Policy, gw *Gateway)bool) {
		for _, p := range(allPolicies) {
			if isAttached(p, gw) {
				policies = append(policies, p)
			}
		}
	}

	// Policies attached to GatewayClass of Gateway
	selectPolicy(func(policy Policy, gw *Gateway)bool { return policy.targetRef.kindNamespacedName == gw.class.kindNamespacedName })

	// Policies attached to Gateway namespace
	selectPolicy(func(policy Policy, gw *Gateway)bool { return policy.targetRef.kind == "Namespace" && policy.targetRef.name == gw.namespace })

	// Policies attached to Gateway
	selectPolicy(func(policy Policy, gw *Gateway)bool { return policy.targetRef.kindNamespacedName == gw.kindNamespacedName })

	p := EffectivePolicy{}
	data := map[string]any{}

	if useGatewayClassParamAsPolicy && gwClassParameterPath != nil && gw.class != nil && gw.class.parameters != nil && len(gw.class.parameters.data)>0 {
		data = gw.class.parameters.data
	}

	// overrides settings operate in a "less specific beats more specific" fashion
	for _, p := range policies {
		if pdef, found := p.spec["default"]; found {
			data = merge(data, pdef).(map[string]any)
		}
	}

	// defaults settings operate in a "more specific beats less specific" fashion
	for idx := len(policies)-1; idx >= 0; idx-- {
		p := policies[idx]
		if povrd, found := p.spec["override"]; found {
			data = merge(data, povrd).(map[string]any)
		}
	}

	p.bodyTxt, p.bodyHtml = commonBodyTextProc(data)
	gw.effPolicy = &p
}

func outputDotGraph(w io.Writer, s *State) {

	fmt.Fprint(w, dot_graph_template_header)

	// Nodes, GatewayClasses and parameters
	fmt.Fprintf(w, dot_cluster_template_header, "cluster_gwc")
	for _, gwc := range s.gwcList {
		var params string = fmt.Sprintf("Controller:<br/>%s", gwc.raw.Spec.ControllerName)
		fmt.Fprintf(w, dot_gatewayclass_template, gwc.id, gwc.name, params)
		if showEffectivePolicies && gwc.effPolicy != nil {
			fmt.Fprintf(w, dot_effpolicy_template, gwc.id+"_effpolicy", "", gwc.effPolicy.bodyHtml)
		}
		if gwc.parameters != nil {
			p := gwc.parameters
			fmt.Fprintf(w, dot_gatewayclassparams_template, p.id, p.groupKind, p.namespacedName, p.bodyHtml)
		}
	}
	fmt.Fprint(w, dot_cluster_template_footer)

	// Nodes, Gateways
	fmt.Fprintf(w, dot_cluster_template_header, "cluster_gw")
	for _, gw := range s.gwList {
		var params string
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
		fmt.Fprintf(w, dot_gateway_template, gw.id, gw.namespacedName, params)
		if showEffectivePolicies && gw.effPolicy != nil {
			fmt.Fprintf(w, dot_effpolicy_template, gw.id+"_effpolicy", "", gw.effPolicy.bodyHtml)
		}
	}
	fmt.Fprint(w, dot_cluster_template_footer)

	// Nodes, HTTPRoutes
	fmt.Fprintf(w, dot_cluster_template_header, "cluster_httproute")
	for _, rt := range s.httpRtList {
		var params string
		if rt.raw.Spec.Hostnames != nil {
			for idx, hname := range rt.raw.Spec.Hostnames {
				if idx > 0 {
					params += "<br/>"
				}
				params += fmt.Sprintf("%s", hname)
			}
		} else {
			params = "<i>(no hostname)</i>"
		}
		fmt.Fprintf(w, dot_httproute_template, rt.id, rt.namespacedName, params)
	}
	fmt.Fprint(w, dot_cluster_template_footer)

	// Nodes, backends
	fmt.Fprintf(w, dot_cluster_template_header, "cluster_backends")
	for _, rt := range s.httpRtList {
		for _, backend := range rt.backends {
			fmt.Fprintf(w, dot_backend_template, backend.id, backend.groupKind, backend.namespacedName, "? endpoint(s)")
		}
	}
	fmt.Fprint(w, dot_cluster_template_footer)

	// Nodes, attached policies
	if showPolicies {
		for _, policy := range s.attachedPolicies {
			fmt.Fprintf(w, dot_policy_template, policy.id, policy.groupKind, policy.namespacedName, policy.bodyHtml)
		}
	}

	// Edges
	for _, gwc := range s.gwcList {
		//if showEffectivePolicies {
		//	fmt.Fprintf(w, "	%s -> %s\n", gwc.id+"_effpolicy", gwc.id)
		//}
		if gwc.parameters != nil {
			fmt.Fprintf(w, "\t%s -> %s\n", gwc.id, gwc.parameters.id)
		}
	}
	for _, gw := range s.gwList {
		if gw.class != nil { // no matching gatewayclass
			fmt.Fprintf(w, "	%s -> %s\n", gw.id, gw.class.id)
			if showEffectivePolicies {
				fmt.Fprintf(w, "	%s -> %s\n", gw.id+"_effpolicy", gw.id)
			}
		}
	}
	for _, rt := range s.httpRtList {
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
	if showPolicies {
		for _, policy := range s.attachedPolicies {
			if policy.targetRef.kind == "Namespace" {  // Namespace policies
				//fmt.Fprintf(w, "\t%s -> %s\n", policy.id, policy.targetRef.id)
				for _, gw := range s.gwList {
					if gw.namespace == policy.targetRef.name && gw.class != nil { // no matching gatewayclass
						fmt.Fprintf(w, "	%s -> %s [style=dashed]\n", policy.id, gw.id)
					}
				}
			} else {
				fmt.Fprintf(w, "\t%s -> %s\n", policy.id, policy.targetRef.id)
			}
		}
	}
	fmt.Fprint(w, dot_graph_template_footer)
}

func outputTxtTablePolicyFocus(s *State) {
	t := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)
	fmt.Fprintln(t, "NAMESPACE\tPOLICY\tTARGET\tDEFAULT\tOVERRIDE")
	for _, policy := range s.attachedPolicies {
		// Does the policy have 'default' or 'override' settings
		def := "No"
		override := "No"
		_, defFound, _ := unstructured.NestedMap(policy.raw.Object, "spec", "default");
		_, overrideFound, _ := unstructured.NestedMap(policy.raw.Object, "spec", "override");
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
	for _, gwc := range s.gwcList {
		fmt.Fprintf(t, "GatewayClass %s\tcontroller:%s\n", gwc.name, gwc.controllerName)
		for _, gw := range s.gwList {
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
			for _, rt := range s.httpRtList {
				for _, pref := range rt.raw.Spec.ParentRefs {
					if IsRefToGateway(pref, gw) {
						fmt.Fprintf(t, "     ├─ HTTPRoute %s\t\n", rt.namespacedName)
						for _,rule := range rt.raw.Spec.Rules {
							fmt.Fprintf(t, "     │   ├─ match\t")
							for _,match := range rule.Matches {
								fmt.Fprintf(t, "%s %s ", *match.Path.Type, *match.Path.Value)
							}
							fmt.Fprintf(t, "\n")
							fmt.Fprintf(t, "     │   ├─ backends\t")
							for _,be := range rule.BackendRefs {
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
	for _, gw := range s.gwList {
		for _, l := range gw.raw.Spec.Listeners {
			if l.Hostname != nil {
				fmt.Fprintf(t, "%s\t", *l.Hostname)
			} else {
				fmt.Fprintf(t, "(none)\t")
			}
		}
		fmt.Fprintf(t, "\n")
		for _, rt := range s.httpRtList {
			for _, pref := range rt.raw.Spec.ParentRefs {
				if IsRefToGateway(pref, gw) {
					for _,rule := range rt.raw.Spec.Rules {
						for _,match := range rule.Matches {
							fmt.Fprintf(t, "  ├─ %s %s\t", *match.Path.Type, *match.Path.Value)
							for _,be := range rule.BackendRefs {
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
	t := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)
	fmt.Fprintln(t, "GATEWAY\tCONFIGURATION")
	for _, gw := range s.gwList {
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

func unstructured2TargetRef(us unstructured.Unstructured) (*gatewayv1a2.PolicyTargetReference, error) {

	targetRef, found, err := unstructured.NestedMap(us.Object, "spec", "targetRef");
	if !found || err!=nil {
		return nil, err
	}
	group, found, err := unstructured.NestedString(targetRef, "group");
	if !found || err!=nil {
		return nil, err
	}
	kind, found, err := unstructured.NestedString(targetRef, "kind");
	if !found || err!=nil {
		return nil, err
	}
	name, found, err := unstructured.NestedString(targetRef, "name");
	if !found || err!=nil {
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

func IsRefToGateway(parentRef gatewayv1b1.ParentReference, gateway Gateway) bool {
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
	return mapping.Scope.Name() ==  meta.RESTScopeNameNamespace, nil
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
	return mapping.Scope.Name() ==  meta.RESTScopeNameNamespace, nil
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

	isNamespaced := mapping.Scope.Name() ==  meta.RESTScopeNameNamespace

	return &mapping.Resource, isNamespaced, nil
}

// Lookup GVR for CRD in unstructured.Unstructured
func crd2gvr(cl client.Client, crd *unstructured.Unstructured) (*schema.GroupVersionResource, error) {
	group, found, err := unstructured.NestedString(crd.Object, "spec", "group")
	if err != nil || !found {
		return nil, fmt.Errorf("Cannot lookup group")
	}
	kind, found, err := unstructured.NestedString(crd.Object, "spec", "names", "kind")
	if err != nil || !found {
		return nil, fmt.Errorf("Cannot lookup kind")
	}
	versions, found, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	versionsMap := versions[0].(map[string]any)
	version := versionsMap["name"].(string)

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

	isNamespaced := mapping.Scope.Name() ==  meta.RESTScopeNameNamespace
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

func gv_node_template(resType string, leftAlignBody bool, colourWheel int) string {
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
	sr := uint8(float64(r)*shade)
	sg := uint8(float64(g)*shade)
	sb := uint8(float64(b)*shade)
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
		fillcolor="#`+fillcolour+`"
		color="#`+edgecolour+`"
		label=<<table border="0" cellborder="1" cellspacing="0" cellpadding="4">
			<tr> <td sides="B"> <b>`+resType+`</b><br/>%s</td> </tr>
			<tr> <td sides="T"`+bodyAlign+`>%s</td> </tr>
		</table>>
	]`+"\n"
}

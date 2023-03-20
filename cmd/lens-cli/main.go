package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"gopkg.in/yaml.v3"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	//corev1 "k8s.io/api/core/v1"
	//"k8s.io/apimachinery/pkg/api/errors"
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

var (
	scheme = runtime.NewScheme()
)

const (
	dot_graph_template_header string = `
digraph gatewayapi_config {
	rankdir = RL
	#graph [
	#	label = "Gateway API Configuration\n\n"
	#	labelloc = t
	#	fontname = "Helvetica,Arial,sans-serif"
	#	fontsize = 20
	#	layout = dot
	#	newrank = true
	#]
	node [
		style=filled
		shape=rect
		color=red
		pencolor="#00000044" // frames color
		fontname="Helvetica,Arial,sans-serif"
		shape=plaintext
	]
	edge [
		arrowsize=0.5
		fontname="Helvetica,Arial,sans-serif"
		labeldistance=3
		labelfontcolor="#00000080"
		penwidth=2
		style=dotted
	]
`
	dot_graph_template_footer string = `}
`
	// Args: id, name, body
	dot_gatewayclass_template = `	%s [
		fillcolor="#0044ff22"
		label=<<table border="0" cellborder="1" cellspacing="0" cellpadding="4">
			<tr> <td> <b>GatewayClass</b><br/>%s</td> </tr>
			<tr> <td>%s</td> </tr>
		</table>>
		shape=plain
	]
`

	// Args: id, param group, param kind, param name, param body
	dot_gatewayclassparams_template = `	%s [
		fillcolor="#0033dd11"
		label=<<table border="0" cellborder="1" cellspacing="0" cellpadding="4">
			<tr> <td> <b>%s/%s</b><br/>%s</td> </tr>
			<tr> <td align="left" balign="left">%s</td> </tr>
		</table>>
		shape=plain
	]
`
	// Args: id, namespacedName, body
	dot_gateway_template = `	%s [
		fillcolor="#ff880022"
		label=<<table border="0" cellborder="1" cellspacing="0" cellpadding="4">
			<tr> <td> <b>Gateway</b><br/>%s</td> </tr>
			<tr> <td>%s</td> </tr>
		</table>>
		shape=plain
	]
`
	// Args: id, namespacedName, body
	dot_httproute_template = `	%s [
		fillcolor="#88ff0022"
		label=<<table border="0" cellborder="1" cellspacing="0" cellpadding="4">
			<tr> <td> <b>HTTPRoute</b><br/>%s</td> </tr>
			<tr> <td>%s</td> </tr>
		</table>>
		shape=plain
	]
`
	dot_backend_template = `	backend_%s_%s [
		fillcolor="#00888822"
		label=<<table border="0" cellborder="1" cellspacing="0" cellpadding="4">
			<tr> <td> <b>%s</b><br/>%s/%s</td> </tr>
			<tr> <td>%s</td> </tr>
		</table>>
		shape=plain
	]
`
	dot_policy_template = `	%s [
		fillcolor="#00ccaa22"
		label=<<table border="0" cellborder="1" cellspacing="0" cellpadding="4">
			<tr> <td> <b>%s</b><br/>%s</td> </tr>
			<tr> <td align="left" balign="left">%s</td> </tr>
		</table>>
		shape=plain
	]
`
	dot_cluster_template = "\tsubgraph %s {\n\trankdir = TB\n\tcolor=none\n"
)

type State struct {
	cl                client.Client
	dcl               *dynamic.DynamicClient
	gwcList           []GatewayClass
	gwList            []Gateway
	httpRtList        []HTTPRoute
	ingressList       *networkingv1.IngressList
	//attachedPolicies  []unstructured.Unstructured
	attachedPolicies  []Policy
}

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
}

// Processed information for GatewayClass resources
type GatewayClass struct {
	CommonObjRef

	controllerName   string
	parameters       *CommonObjRef

	// The unprocessed resource
	raw *gatewayv1b1.GatewayClass

	// The unprocessed parameters
	rawParams *unstructured.Unstructured
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

	// The unprocessed resource
	raw *gatewayv1b1.HTTPRoute
}

// Processed information for policy resources
type Policy struct {
	CommonObjRef

	// Policy targetRef read from unstructured
	targetRef *gatewayv1a2.PolicyTargetReference

	// Whether the policy is namespaced
	isNamespaced bool
	// Whether the target is namespaced
	targetIsNamespaced bool

	// 'Namespace/name' or 'Name' of target resource, depending on whether target is namespaced or cluster scoped
	targetNamespacedName string

	// 'Kind/Namespace/name' or 'Kind/Name' of target resource, depending on whether target is namespaced or cluster scoped
	targetKindNamespacedName string

	// Unique target ID from a combination of kind, namespace (if applicable) and resource name
	targetId string

	// The unprocessed resource
	raw *unstructured.Unstructured
}

type KubeObj interface {
	GetName() string
	GetNamespace() string
	GetObjectKind() schema.ObjectKind
}

func main() {
	log.Printf("version: %s\n", version.Version)

	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1b1.AddToScheme(scheme))
	utilruntime.Must(apiextensions.AddToScheme(scheme))

	var kubeconfig *string
	var outputFormat *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	outputFormat = flag.String("o", "policy", "output format [policy|graph|hierarchy|route-tree]")
	flag.Parse()

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

	cl, _ := client.New(config, client.Options{
		Scheme: scheme,
	})
	dcl := dynamic.NewForConfigOrDie(config)

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

	state := State{cl, dcl, nil, nil, nil, ingressList, nil}

	//
	// Pre-process resources - common processing to make output
	// functions simpler by e.g. pre-formatting strings
	//

	// Pre-process GatewayClass resources
	for _, gwc := range gwcList.Items {
		gwclass := GatewayClass{}
		commonPreProc(&gwclass.CommonObjRef, &gwc, false)
		gwclass.raw = gwc.DeepCopy()
		gwclass.controllerName = string(gwc.Spec.ControllerName)
		if gwc.Spec.ParametersRef != nil {
			objref := &CommonObjRef{}
			objref.name = gwc.Spec.ParametersRef.Name
			objref.kind = string(gwc.Spec.ParametersRef.Kind)
			objref.group = string(gwc.Spec.ParametersRef.Group)
			if gwc.Spec.ParametersRef.Namespace != nil {
				objref.isNamespaced = true
				objref.namespace = string(*gwc.Spec.ParametersRef.Namespace)
			} else {
				objref.isNamespaced = false
			}
			commonObjRefPreProc(objref)
			gwclass.parameters = objref
			gwclass.rawParams, err = getGatewayParameters(cl, dcl, &gwclass)
			if err != nil {
				log.Printf("Cannot lookup GatewayClass parameter: %s\n", objref.kindNamespacedName)
			} else {
				body,found,err := unstructured.NestedMap(gwclass.rawParams.Object, "data")
				if err != nil {
					log.Fatalf("Cannot lookup GatewayClass parameter body: %w", err)
				}
				if found {
					//delete(spec, "targetRef")
					commonBodyTextProc(gwclass.parameters, body)
				}
			}
		}
		state.gwcList = append(state.gwcList, gwclass)
	}

	// Pre-process Gateway resources
	for _, gw := range gwList.Items {
		g := Gateway{}
		commonPreProc(&g.CommonObjRef, &gw, true)
		g.raw = gw.DeepCopy()
		g.class = state.gatewayclassByName(string(gw.Spec.GatewayClassName))
		state.gwList = append(state.gwList, g)
	}

	// Pre-process HTTPRoute resources
	for _, rt := range httpRtList.Items {
		r := HTTPRoute{}
		commonPreProc(&r.CommonObjRef, &rt, true)
		r.raw = rt.DeepCopy()
		state.httpRtList = append(state.httpRtList, r)
	}

	// Pre-process policy resource
	for _, policy := range attachedPolicies {
		pol := Policy{}
		pol.raw = policy.DeepCopy()
		_, isNamespaced, err := unstructured2gvr(cl, &policy)
		if err != nil {
			log.Fatalf("Cannot lookup GVR of %+v: %w", policy, err)
		}
		commonPreProc(&pol.CommonObjRef, &policy, isNamespaced)
		pol.targetRef, err = unstructured2TargetRef(policy)
		if err != nil {
			log.Fatalf("Cannot convert policy to targetRef: %w", err)
		}
		pol.targetIsNamespaced, err = isNamespacedTargetRef(cl, pol.targetRef)
		if err != nil {
			log.Fatalf("Cannot lookup namespace scope for targetRef %+v, policy %+v", pol.targetRef, policy)
		}
		if pol.targetIsNamespaced {
			pol.targetNamespacedName = fmt.Sprintf("%s/%s",  *pol.targetRef.Namespace, pol.targetRef.Name)
			pol.targetKindNamespacedName = fmt.Sprintf("%s/%s/%s", pol.targetRef.Kind,  *pol.targetRef.Namespace, pol.targetRef.Name)
			pol.targetId = fmt.Sprintf("%s_%s_%s", pol.targetRef.Kind,
				strings.ReplaceAll(string(*pol.targetRef.Namespace), "-", "_"),
				strings.ReplaceAll(string(pol.targetRef.Name), "-", "_"))
		} else {
			pol.targetNamespacedName = string(pol.targetRef.Name)
			pol.targetKindNamespacedName = fmt.Sprintf("%s/%s", pol.targetRef.Kind, pol.targetRef.Name)
			pol.targetId = fmt.Sprintf("%s_%s", pol.targetRef.Kind,
				strings.ReplaceAll(string(pol.targetRef.Name), "-", "_"))
		}
		spec,found,err := unstructured.NestedMap(policy.Object, "spec")
		if err != nil {
			log.Fatalf("Cannot lookup policy spec: %w", err)
		}
		if found {
			delete(spec, "targetRef")
			commonBodyTextProc(&pol.CommonObjRef, spec)
		}
		state.attachedPolicies = append(state.attachedPolicies, pol)
	}

	switch *outputFormat {
	case "policy":
		outputTxtTablePolicyFocus(&state)
	case "graph":
		outputDotGraph(&state)
	case "hierarchy":
		outputTxtClassHierarchy(&state)
	case "route-tree":
		outputTxtRouteTree(&state)
	//outputTxtTableGatewayFocus(&state)
	}
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
	c.groupKind = fmt.Sprintf("%s/%s", c.group, c.kind)
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

func commonBodyTextProc(c *CommonObjRef, body map[string]any) {
	b, err := yaml.Marshal(body)
	if err != nil {
		log.Fatalf("Cannot marshal resource body selection: %w", err)
	}
	c.bodyTxt = string(b)
	c.bodyHtml = ""
	for _,ln := range strings.Split(c.bodyTxt, "\n") {
		if ln != "" {
			c.bodyHtml += fmt.Sprintf("%s<br/>", strings.ReplaceAll(ln, " ", "<i> </i>"))
		}
	}
}

func outputDotGraph(s *State) {

	fmt.Print(dot_graph_template_header)

	// Nodes, GatewayClasses and parameters
	fmt.Printf(dot_cluster_template, "cluster_gwc")
	for _, gwc := range s.gwcList {
		var params string = fmt.Sprintf("Controller:<br/>%s", gwc.raw.Spec.ControllerName)
		fmt.Printf(dot_gatewayclass_template, gwc.id, gwc.name, params)
		if gwc.parameters != nil {
			p := gwc.parameters
			fmt.Printf(dot_gatewayclassparams_template, p.id, p.group, p.kind, p.namespacedName, p.bodyHtml)
		}
	}
	fmt.Print("\t}\n")

	// Nodes, Gateways
	fmt.Printf(dot_cluster_template, "cluster_gw")
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
		fmt.Printf(dot_gateway_template, gw.id, gw.namespacedName, params)
	}
	fmt.Print("\t}\n")

	// Nodes, HTTPRoutes
	fmt.Printf(dot_cluster_template, "cluster_httproute")
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
		fmt.Printf(dot_httproute_template, rt.id, rt.namespacedName, params)
	}
	fmt.Print("\t}\n")

	// Nodes, backends
	fmt.Printf(dot_cluster_template, "cluster_backends")
	for _, rt := range s.httpRtList {
		for _, rules := range rt.raw.Spec.Rules {
			for _, backend := range rules.BackendRefs {
				fmt.Printf(dot_backend_template, strings.ReplaceAll(string(Deref(backend.Namespace, gatewayv1b1.Namespace(rt.raw.ObjectMeta.Namespace))), "-", "_"), strings.ReplaceAll(string(backend.Name), "-", "_"), Deref(backend.Kind, "Service"), Deref(backend.Namespace, gatewayv1b1.Namespace(rt.raw.ObjectMeta.Namespace)), backend.Name, "? endpoint(s)")
			}
		}
	}
	fmt.Print("\t}\n")

	// Nodes, attached policies
	for _, policy := range s.attachedPolicies {
		fmt.Printf(dot_policy_template, policy.id, policy.groupKind, policy.namespacedName, policy.bodyHtml)
	}

	// Edges
	for _, gwc := range s.gwcList {
		if gwc.parameters != nil {
			fmt.Printf("\t%s -> %s\n", gwc.id, gwc.parameters.id)
		}
	}
	for _, gw := range s.gwList {
		if gw.class != nil { // no matching gatewayclass
			fmt.Printf("	%s -> %s\n", gw.id, gw.class.id)
		}
	}
	for _, rt := range s.httpRtList {
		for _, pref := range rt.raw.Spec.ParentRefs {
			ns := rt.raw.ObjectMeta.Namespace
			if pref.Namespace != nil {
				ns = string(*pref.Namespace)
			}
			if pref.Kind != nil && *pref.Kind == gatewayv1b1.Kind("Gateway") {
				fmt.Printf("	%s -> Gateway_%s_%s\n", rt.id, strings.ReplaceAll(ns, "-", "_"), strings.ReplaceAll(string(pref.Name), "-", "_"))
			}
		}
		for _, rules := range rt.raw.Spec.Rules {
			for _, backend := range rules.BackendRefs {
				fmt.Printf("	backend_%s_%s -> %s\n", strings.ReplaceAll(string(Deref(backend.Namespace, gatewayv1b1.Namespace(rt.raw.ObjectMeta.Namespace))), "-", "_"), strings.ReplaceAll(string(backend.Name), "-", "_"), rt.id)
			}
		}
	}
	// Edges, attached policies
	for _, policy := range s.attachedPolicies {
		fmt.Printf("\t%s -> %s\n", policy.id, policy.targetId)
	}
	fmt.Print(dot_graph_template_footer)
}

func outputTxtTableGatewayFocus(s *State) {
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
			fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\n", policy.namespace, policy.kindName, policy.targetKindNamespacedName, def, override)
		} else {
			fmt.Fprintf(t, "\t%s\t%s\t%s\t%s\n", policy.kindName, policy.targetKindNamespacedName, def, override)
		}
	}
	t.Flush()
}

// Hierarchy with GatewayClass at the top
func outputTxtClassHierarchy(s *State) {
	t := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)
	fmt.Fprintln(t, "RESOURCE\tCONFIGURATION")
	for _, gwc := range s.gwcList {
		fmt.Fprintf(t, "GatewayClass %s\t\n", gwc.name)
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

func Deref[T any](ptr *T, deflt T) T {
	if ptr != nil {
		return *ptr
	}
	return deflt
}

func PtrTo[T any](val T) *T {
	return &val
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

// func NamespacedNameOf(obj metav1.Object) types.NamespacedName {
// 	name := types.NamespacedName{
// 		Name:	   obj.GetName(),
// 		Namespace: obj.GetNamespace(),
// 	}

// 	if name.Namespace == "" {
// 		name.Namespace = metav1.NamespaceDefault
// 	}

// 	return name
// }

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

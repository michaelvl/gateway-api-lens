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

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	//corev1 "k8s.io/api/core/v1"
	//"k8s.io/apimachinery/pkg/api/errors"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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

	client "sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
	gatewayv1b1 "sigs.k8s.io/gateway-api/apis/v1beta1"
	"github.com/michaelvl/gateway-api-lens/pkg/version"
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
	dot_gatewayclass_template = `	GatewayClass_%s [
		fillcolor="#0044ff22"
		label=<<table border="0" cellborder="1" cellspacing="0" cellpadding="4">
			<tr> <td> <b>GatewayClass</b><br/>%s</td> </tr>
			<tr> <td>%s</td> </tr>
		</table>>
		shape=plain
	]
`
	dot_gatewayclassparams_template = `	gwcp_%s [
		fillcolor="#0033dd11"
		label=<<table border="0" cellborder="1" cellspacing="0" cellpadding="4">
			<tr> <td> <b>%s/%s</b><br/>%s</td> </tr>
			<tr> <td>%s</td> </tr>
		</table>>
		shape=plain
	]
`
	dot_gateway_template = `	Gateway_%s_%s [
		fillcolor="#ff880022"
		label=<<table border="0" cellborder="1" cellspacing="0" cellpadding="4">
			<tr> <td> <b>Gateway</b><br/>%s/%s</td> </tr>
			<tr> <td>%s</td> </tr>
		</table>>
		shape=plain
	]
`
	dot_httproute_template = `	HTTPRoute_%s_%s [
		fillcolor="#88ff0022"
		label=<<table border="0" cellborder="1" cellspacing="0" cellpadding="4">
			<tr> <td> <b>HTTPRoute</b><br/>%s/%s</td> </tr>
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
	dot_policy_template = `	policy_%s [
		fillcolor="#00ccaa22"
		label=<<table border="0" cellborder="1" cellspacing="0" cellpadding="4">
			<tr> <td> <b>%s</b><br/>%s</td> </tr>
			<tr> <td>%s</td> </tr>
		</table>>
		shape=plain
	]
`
	dot_cluster_template = "\tsubgraph %s {\n\trankdir = TB\n\tcolor=none\n"
)

type State struct {
	cl                client.Client
	dcl               *dynamic.DynamicClient
	gwcList           *gatewayv1b1.GatewayClassList
	gwList            *gatewayv1b1.GatewayList
	httpRtList        *gatewayv1b1.HTTPRouteList
	ingressList       *networkingv1.IngressList
	//attachedPolicies  []unstructured.Unstructured
	attachedPolicies  []Policy
}

// Processed resources
type Policy struct {
	// The unprocessed resource
	raw unstructured.Unstructured

	// Whether the policy CRD is namespaced
	isNamespaced bool

	// Policy namespace
	name string

	// Policy kind/name
	kindName string


	// Policy namespace
	namespace string

	// 'Namespace/name' or 'Name' of policy resource, depending on whether policy CRD is namespaced or cluster scoped
	namespacedName string

	// group+kind separated by '/'
	groupKind string

	// A unique resource ID from a combination of kind, namespace (if applicable) and resource name
	id string

	// Policy targetRef read from unstructured
	targetRef *gatewayv1a2.PolicyTargetReference

	// Whether the target is namespaced
	targetIsNamespaced bool

	// 'Namespace/name' or 'Name' of target resource, depending on whether target is namespaced or cluster scoped
	targetNamespacedName string

	// 'Kind/Namespace/name' or 'Kind/Name' of target resource, depending on whether target is namespaced or cluster scoped
	targetKindNamespacedName string

	// Unique target ID from a combination of kind, namespace (if applicable) and resource name
	targetId string
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
	outputFormat = flag.String("o", "policy", "output format [policy|graph|gatewayclass-hierarchy]")
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

	state := State{cl, dcl, gwcList, gwList, httpRtList, ingressList, nil}

	// Pre-process resources - common processing to make output functions simpler

	for _, policy := range attachedPolicies {
		pol := Policy{}
		pol.raw = policy
		gvr, isNamespaced, err := unstructured2gvr(cl, &policy)
		if err != nil {
			log.Fatalf("Cannot lookup GVR of %+v: %w", policy, err)
		}
		pol.groupKind = fmt.Sprintf("%s/%s", gvr.Group, policy.GetKind())
		pol.isNamespaced = isNamespaced
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
			pol.targetId = fmt.Sprintf("%s_%s", pol.targetRef.Kind,
				strings.ReplaceAll(string(pol.targetRef.Name), "-", "_"))
		}
		pol.name = policy.GetName()
		pol.namespace = policy.GetNamespace()
		pol.kindName = fmt.Sprintf("%s/%s", policy.GetKind(), policy.GetName())
		if isNamespaced {
			pol.namespacedName = fmt.Sprintf("%s/%s", policy.GetNamespace(), policy.GetName())
			pol.id = fmt.Sprintf("%s_%s_%s", policy.GetKind(), strings.ReplaceAll(policy.GetNamespace(), "-", "_"),
				strings.ReplaceAll(policy.GetName(), "-", "_"))
		} else {
			pol.namespacedName = policy.GetName()
			pol.id = fmt.Sprintf("%s_%s", policy.GetKind(), strings.ReplaceAll(policy.GetName(), "-", "_"))
		}
		state.attachedPolicies = append(state.attachedPolicies, pol)
	}

	switch *outputFormat {
	case "policy":
		outputTxtTablePolicyFocus(&state)
	case "graph":
		outputDotGraph(&state)
	case "gatewayclass-hierarchy":
		outputTxtClassHierarchy(&state)
	//outputTxtTableGatewayFocus(&state)
	}
}

func outputDotGraph(s *State) {

	fmt.Print(dot_graph_template_header)

	// Nodes, GatewayClasses and parameters
	fmt.Printf(dot_cluster_template, "cluster_gwc")
	for _, gwc := range s.gwcList.Items {
		var params string = fmt.Sprintf("Controller:<br/>%s", gwc.Spec.ControllerName)
		fmt.Printf(dot_gatewayclass_template, strings.ReplaceAll(gwc.ObjectMeta.Name, "-", "_"), gwc.ObjectMeta.Name, params)
		if gwc.Spec.ParametersRef != nil {
			fmt.Printf(dot_gatewayclassparams_template, strings.ReplaceAll(gwc.Spec.ParametersRef.Name, "-", "_"), gwc.Spec.ParametersRef.Group, gwc.Spec.ParametersRef.Kind, gwc.Spec.ParametersRef.Name, "-")
		}
	}
	fmt.Print("\t}\n")

	// Nodes, Gateways
	fmt.Printf(dot_cluster_template, "cluster_gw")
	for _, gw := range s.gwList.Items {
		var params string
		for idx, l := range gw.Spec.Listeners {
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
		fmt.Printf(dot_gateway_template, strings.ReplaceAll(gw.ObjectMeta.Namespace, "-", "_"), strings.ReplaceAll(gw.ObjectMeta.Name, "-", "_"), gw.ObjectMeta.Namespace, gw.ObjectMeta.Name, params)
	}
	fmt.Print("\t}\n")

	// Nodes, HTTPRoutes
	fmt.Printf(dot_cluster_template, "cluster_httproute")
	for _, rt := range s.httpRtList.Items {
		var params string
		if rt.Spec.Hostnames != nil {
			for idx, hname := range rt.Spec.Hostnames {
				if idx > 0 {
					params += "<br/>"
				}
				params += fmt.Sprintf("%s", hname)
			}
		} else {
			params = "<i>(no hostname)</i>"
		}
		fmt.Printf(dot_httproute_template, strings.ReplaceAll(rt.ObjectMeta.Namespace, "-", "_"), strings.ReplaceAll(rt.ObjectMeta.Name, "-", "_"), rt.ObjectMeta.Namespace, rt.ObjectMeta.Name, params)
	}
	fmt.Print("\t}\n")

	// Nodes, backends
	fmt.Printf(dot_cluster_template, "cluster_backends")
	for _, rt := range s.httpRtList.Items {
		for _, rules := range rt.Spec.Rules {
			for _, backend := range rules.BackendRefs {
				fmt.Printf(dot_backend_template, strings.ReplaceAll(string(Deref(backend.Namespace, gatewayv1b1.Namespace(rt.ObjectMeta.Namespace))), "-", "_"), strings.ReplaceAll(string(backend.Name), "-", "_"), Deref(backend.Kind, "Service"), Deref(backend.Namespace, gatewayv1b1.Namespace(rt.ObjectMeta.Namespace)), backend.Name, "? endpoint(s)")
			}
		}
	}
	fmt.Print("\t}\n")

	// Nodes, attached policies
	for _, policy := range s.attachedPolicies {
		fmt.Printf(dot_policy_template, policy.id, policy.groupKind, policy.namespacedName, "-")
	}

	// Edges
	for _, gwc := range s.gwcList.Items {
		if gwc.Spec.ParametersRef != nil {
			fmt.Printf("\tGatewayClass_%s -> gwcp_%s\n", strings.ReplaceAll(string(gwc.ObjectMeta.Name), "-", "_"), strings.ReplaceAll(gwc.Spec.ParametersRef.Name, "-", "_"))
		}
	}
	for _, gw := range s.gwList.Items {
		fmt.Printf("	Gateway_%s_%s -> GatewayClass_%s\n", strings.ReplaceAll(gw.ObjectMeta.Namespace, "-", "_"), strings.ReplaceAll(gw.ObjectMeta.Name, "-", "_"), strings.ReplaceAll(string(gw.Spec.GatewayClassName), "-", "_"))
	}
	for _, rt := range s.httpRtList.Items {
		for _, pref := range rt.Spec.ParentRefs {
			ns := rt.ObjectMeta.Namespace
			if pref.Namespace != nil {
				ns = string(*pref.Namespace)
			}
			if pref.Kind != nil && *pref.Kind == gatewayv1b1.Kind("Gateway") {
				fmt.Printf("	HTTPRoute_%s_%s -> Gateway_%s_%s\n", strings.ReplaceAll(rt.ObjectMeta.Namespace, "-", "_"), strings.ReplaceAll(rt.ObjectMeta.Name, "-", "_"), strings.ReplaceAll(ns, "-", "_"), strings.ReplaceAll(string(pref.Name), "-", "_"))
			}
		}
		for _, rules := range rt.Spec.Rules {
			for _, backend := range rules.BackendRefs {
				fmt.Printf("	backend_%s_%s -> HTTPRoute_%s_%s\n", strings.ReplaceAll(string(Deref(backend.Namespace, gatewayv1b1.Namespace(rt.ObjectMeta.Namespace))), "-", "_"), strings.ReplaceAll(string(backend.Name), "-", "_"), strings.ReplaceAll(rt.ObjectMeta.Namespace, "-", "_"), strings.ReplaceAll(rt.ObjectMeta.Name, "-", "_"))
			}
		}
	}
	// Edges, attached policies
	for _, policy := range s.attachedPolicies {
		fmt.Printf("\tpolicy_%s -> %s\n", policy.id, policy.targetId)
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
	for _, gwc := range s.gwcList.Items {
		fmt.Printf("GatewayClass %s (controller:%s)\n", gwc.ObjectMeta.Name, gwc.Spec.ControllerName)
		for _, gw := range s.gwList.Items {
			if string(gw.Spec.GatewayClassName) != gwc.ObjectMeta.Name {
				continue
			}
			fmt.Printf("  Gateway %s/%s:", gw.ObjectMeta.Namespace, gw.ObjectMeta.Name)
			for _, l := range gw.Spec.Listeners {
				fmt.Printf(" %s:%s/%v", l.Name, l.Protocol, l.Port)
				if l.Hostname != nil {
					fmt.Printf(" %s", *l.Hostname)
				}
			}
			fmt.Printf("\n")
			for _, rt := range s.httpRtList.Items {
				for _, pref := range rt.Spec.ParentRefs {
					if IsRefToGateway(pref, NamespacedNameOf(&gw)) {
						fmt.Printf("    HTTPRoute %s/%s\n", rt.ObjectMeta.Namespace, rt.ObjectMeta.Name)
					}
				}
			}
		}
	}
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

func IsRefToGateway(parentRef gatewayv1b1.ParentReference, gateway types.NamespacedName) bool {
	if parentRef.Group != nil && string(*parentRef.Group) != gatewayv1b1.GroupName {
		return false
	}

	if parentRef.Kind != nil && string(*parentRef.Kind) != "Gateway" {
		return false
	}

	if parentRef.Namespace != nil && string(*parentRef.Namespace) != gateway.Namespace {
		return false
	}

	return string(parentRef.Name) == gateway.Name
}

func NamespacedNameOf(obj metav1.Object) types.NamespacedName {
	name := types.NamespacedName{
		Name:	   obj.GetName(),
		Namespace: obj.GetNamespace(),
	}

	if name.Namespace == "" {
		name.Namespace = metav1.NamespaceDefault
	}

	return name
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

	return &schema.GroupVersionResource{
		Group:	  gk.Group,
		Version:  gv.Version,
		Resource: mapping.Resource.Resource,
	}, isNamespaced, nil
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

	return &schema.GroupVersionResource{
		Group:	  group,
		Version:  version,
		Resource: mapping.Resource.Resource,
	}, nil
}

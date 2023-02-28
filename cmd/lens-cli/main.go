package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	//corev1 "k8s.io/api/core/v1"
	//"k8s.io/apimachinery/pkg/api/errors"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
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
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
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
	dot_policy_template = `	policy_%s_%s [
		fillcolor="#00ccaa22"
		label=<<table border="0" cellborder="1" cellspacing="0" cellpadding="4">
			<tr> <td> <b>%s</b><br/>%s/%s</td> </tr>
			<tr> <td>%s</td> </tr>
		</table>>
		shape=plain
	]
`
	dot_cluster_template = "\tsubgraph %s {\n\trankdir = TB\n\tcolor=none\n"
)

func main() {
	log.Printf("version: %s\n", version.Version)

	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1beta1.AddToScheme(scheme))
	utilruntime.Must(apiextensions.AddToScheme(scheme))

	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
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

	gwcList := &gatewayv1beta1.GatewayClassList{}
	err = cl.List(context.TODO(), gwcList, client.InNamespace(""))

	gwList := &gatewayv1beta1.GatewayList{}
	err = cl.List(context.TODO(), gwList, client.InNamespace(""))

	httpRtList := &gatewayv1beta1.HTTPRouteList{}
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
	//fmt.Printf("Found %d CRDs\n", len(crdDefList.Items))
	for _, crd := range crdDefList.Items {
		gvr, _ := crd2gvr(cl, &crd)
		//fmt.Printf("  %s: %+v\n", crd.GetName(), gvr)
		crdList, err := dcl.Resource(*gvr).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			log.Fatalf("Cannot list CRD %v: %v", gvr, err)
		}
		//fmt.Printf("Found %d CRD instances\n", len(crdList.Items))
		for _, crdInst := range crdList.Items {
			//fmt.Printf("  %s/%s\n", crdInst.GetNamespace(), crdInst.GetName())
			_, found, err := unstructured.NestedMap(crdInst.Object, "spec", "targetRef")
			if err != nil || !found {
				log.Fatalf("Missing or invalid targetRef, not a valid attached policy: %s/%s", crdInst.GetNamespace(), crdInst.GetName())
			}
			attachedPolicies = append(attachedPolicies, crdInst)
			//fmt.Printf("  targetRef: %+v\n", targetRef)
		}
	}

	//svcList := &corev1.ServiceList{}
	//err = cl.List(context.TODO(), svcList, client.InNamespace(""))

	fmt.Print(dot_graph_template_header)

	// Nodes, GatewayClasses and parameters
	fmt.Printf(dot_cluster_template, "cluster_gwc")
	for _, gwc := range gwcList.Items {
		var params string = fmt.Sprintf("Controller:<br/>%s", gwc.Spec.ControllerName)
		fmt.Printf(dot_gatewayclass_template, strings.ReplaceAll(gwc.ObjectMeta.Name, "-", "_"), gwc.ObjectMeta.Name, params)
		if gwc.Spec.ParametersRef != nil {
			fmt.Printf(dot_gatewayclassparams_template, strings.ReplaceAll(gwc.Spec.ParametersRef.Name, "-", "_"), gwc.Spec.ParametersRef.Group, gwc.Spec.ParametersRef.Kind, gwc.Spec.ParametersRef.Name, "-")
		}
	}
	fmt.Print("\t}\n")

	// Nodes, Gateways
	fmt.Printf(dot_cluster_template, "cluster_gw")
	for _, gw := range gwList.Items {
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
	for _, rt := range httpRtList.Items {
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
	for _, rt := range httpRtList.Items {
		for _, rules := range rt.Spec.Rules {
			for _, backend := range rules.BackendRefs {
				fmt.Printf(dot_backend_template, strings.ReplaceAll(string(Deref(backend.Namespace, gatewayv1beta1.Namespace(rt.ObjectMeta.Namespace))), "-", "_"), strings.ReplaceAll(string(backend.Name), "-", "_"), Deref(backend.Kind, "Service"), Deref(backend.Namespace, gatewayv1beta1.Namespace(rt.ObjectMeta.Namespace)), backend.Name, "? endpoint(s)")
			}
		}
	}
	fmt.Print("\t}\n")

	// Nodes, attached policies
	for _, policy := range attachedPolicies {
		fmt.Printf(dot_policy_template, strings.ReplaceAll(policy.GetNamespace(), "-", "_"), strings.ReplaceAll(policy.GetName(), "-", "_"), policy.GetKind(), policy.GetNamespace(), policy.GetName(), "-")
	}

	// Edges
	for _, gwc := range gwcList.Items {
		if gwc.Spec.ParametersRef != nil {
			fmt.Printf("\tGatewayClass_%s -> gwcp_%s\n", strings.ReplaceAll(string(gwc.ObjectMeta.Name), "-", "_"), strings.ReplaceAll(gwc.Spec.ParametersRef.Name, "-", "_"))
		}
	}
	for _, gw := range gwList.Items {
		fmt.Printf("	Gateway_%s_%s -> GatewayClass_%s\n", strings.ReplaceAll(gw.ObjectMeta.Namespace, "-", "_"), strings.ReplaceAll(gw.ObjectMeta.Name, "-", "_"), strings.ReplaceAll(string(gw.Spec.GatewayClassName), "-", "_"))
	}
	for _, rt := range httpRtList.Items {
		for _, pref := range rt.Spec.ParentRefs {
			ns := rt.ObjectMeta.Namespace
			if pref.Namespace != nil {
				ns = string(*pref.Namespace)
			}
			if pref.Kind != nil && *pref.Kind == gatewayv1beta1.Kind("Gateway") {
				fmt.Printf("	HTTPRoute_%s_%s -> Gateway_%s_%s\n", strings.ReplaceAll(rt.ObjectMeta.Namespace, "-", "_"), strings.ReplaceAll(rt.ObjectMeta.Name, "-", "_"), strings.ReplaceAll(ns, "-", "_"), strings.ReplaceAll(string(pref.Name), "-", "_"))
			}
		}
		for _, rules := range rt.Spec.Rules {
			for _, backend := range rules.BackendRefs {
				fmt.Printf("	backend_%s_%s -> HTTPRoute_%s_%s\n", strings.ReplaceAll(string(Deref(backend.Namespace, gatewayv1beta1.Namespace(rt.ObjectMeta.Namespace))), "-", "_"), strings.ReplaceAll(string(backend.Name), "-", "_"), strings.ReplaceAll(rt.ObjectMeta.Namespace, "-", "_"), strings.ReplaceAll(rt.ObjectMeta.Name, "-", "_"))
			}
		}
	}
	// Edges, attached policies
	for _, policy := range attachedPolicies {
		targetRef, _, _ := unstructured.NestedMap(policy.Object, "spec", "targetRef")
		kind, _, _ := unstructured.NestedString(targetRef, "kind")
		name, _, _ := unstructured.NestedString(targetRef, "name")
		namespace, found, _ := unstructured.NestedString(targetRef, "namespace")
		if !found {
			namespace = policy.GetNamespace()
		}
		fmt.Printf("\tpolicy_%s_%s -> %s_%s_%s\n", strings.ReplaceAll(policy.GetNamespace(), "-", "_"), strings.ReplaceAll(policy.GetName(), "-", "_"), kind, strings.ReplaceAll(namespace, "-", "_"), strings.ReplaceAll(name, "-", "_"))
	}
	fmt.Print(dot_graph_template_footer)

	// for _, gwc := range gwcList.Items {
	// 	fmt.Printf("GatewayClass %s (controller:%s)\n", gwc.ObjectMeta.Name, gwc.Spec.ControllerName)
	// 	for _, gw := range gwList.Items {
	// 		if string(gw.Spec.GatewayClassName) != gwc.ObjectMeta.Name {
	// 			continue
	// 		}
	// 		fmt.Printf("  Gateway %s/%s:", gw.ObjectMeta.Namespace, gw.ObjectMeta.Name)
	// 		for _, l := range gw.Spec.Listeners {
	// 			fmt.Printf(" %s:%s/%v", l.Name, l.Protocol, l.Port)
	// 			if l.Hostname != nil {
	// 				fmt.Printf(" %s", *l.Hostname)
	// 			}
	// 		}
	// 		fmt.Printf("\n")
	// 		for _, rt := range httpRtList.Items {
	// 			for _, pref := range rt.Spec.ParentRefs {
	// 				if IsRefToGateway(pref, NamespacedNameOf(&gw)) {
	// 					fmt.Printf("    HTTPRoute %s/%s\n", rt.ObjectMeta.Namespace, rt.ObjectMeta.Name)
	// 				}
	// 			}
	// 		}
	// 	}
	// }
}

func Deref[T any](ptr *T, deflt T) T {
	if ptr != nil {
		return *ptr
	}
	return deflt
}

func IsRefToGateway(parentRef gatewayv1beta1.ParentReference, gateway types.NamespacedName) bool {
	if parentRef.Group != nil && string(*parentRef.Group) != gatewayv1beta1.GroupName {
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

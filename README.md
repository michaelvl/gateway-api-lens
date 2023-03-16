# Gateway API Lens

**Gateway API Lens** is a tool to visualize [Kubernetes Gateway
API](https://gateway-api.sigs.k8s.i) configuration.

The following example from
[gateway-API](https://gateway-api.sigs.k8s.io) will be used to
illustrate the different outputs generated:

![Gateway-API example](doc/images/gateway-roles.png)

# Graphviz Graph Output

```bash
$ bin/linux_amd64/lens-cli -o graph  |  dot -Tsvg > output.svg
```

The is an example where service policies (see
[GEP-713](https://gateway-api.sigs.k8s.io/geps/gep-713)) are attached
to both `GatewayClass` and `Gateway` resources:

![Example Graphviz output](doc/images/graphviz-output.png)

# Policies in Table Format

```bash
$ bin/linux_amd64/lens-cli -o policy

NAMESPACE POLICY                                                   TARGET                        DEFAULT OVERRIDE
          ACMEClusterServicePolicy/acmeclusterservicepolicy-sample GatewayClass/cloud-gw         No      Yes
foo-infra ACMEServicePolicy/acmeservicepolicy-sample               Gateway/foo-infra/foo-gateway Yes     No
foo-infra ACMEServicePolicy/acmeservicepolicy-sample2              GatewayClass/cloud-gw         Yes     No
```

# Hierarchy Format

```bash
$ bin/linux_amd64/lens-cli -o hierarchy

RESOURCE                              CONFIGURATION
GatewayClass cloud-gw
 └─ Gateway foo-infra/foo-gateway     web:HTTP/80 foo.example.com
     ├─ HTTPRoute foo-site/foo-site
     │   ├─ match                     PathPrefix /site
     │   └─ backends                  Service/foo-site/foo-site:80@1
     └─ HTTPRoute foo-store/foo-store
         ├─ match                     PathPrefix /store
         └─ backends                  Service/foo-store-v1:80@90 Service/foo-store-v2:80@10
```

# Route-tree Format

```bash
$ bin/linux_amd64/lens-cli -o route-tree

HOSTNAME/MATCH         BACKEND
foo.example.com
  ├─ PathPrefix /site  Service/foo-site/foo-site:80@1
  ├─ PathPrefix /store Service/foo-store-v1:80@90 Service/foo-store-v2:80@10
```

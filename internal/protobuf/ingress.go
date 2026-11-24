/*
Copyright The Kubernetes Authors.
Copyright 2026 The Shearwater Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

Schema metadata is derived from Kubernetes v1.36.6 networking/v1 generated.proto
and core/v1 generated.proto:
https://github.com/kubernetes/kubernetes/tree/v1.36.6/staging/src/k8s.io/api/networking/v1
*/

package protobuf

func init() {
	roots["networking.k8s.io/v1 Ingress"] = ingress
	roots["networking.k8s.io/v1 IngressClass"] = ingressClass
}

// ---- networking.k8s.io/v1 ----

var ingress = &message{"Ingress", map[int]field{
	1: msg("metadata", objectMeta),
	2: msg("spec", ingressSpec),
	3: status(),
}}

var ingressSpec = &message{"IngressSpec", map[int]field{
	1: msgPtr("defaultBackend", ingressBackend),
	2: msgs("tls", ingressTLS),
	3: msgs("rules", ingressRule),
	4: strPtr("ingressClassName"),
}}

var ingressTLS = &message{"IngressTLS", map[int]field{
	1: strs("hosts"),
	2: str("secretName"),
}}

var ingressRule = &message{"IngressRule", map[int]field{
	1: str("host"),
	2: inline("ingressRuleValue", ingressRuleValue),
}}

var ingressRuleValue = &message{"IngressRuleValue", map[int]field{
	1: msgPtr("http", httpIngressRuleValue),
}}

var httpIngressRuleValue = &message{"HTTPIngressRuleValue", map[int]field{
	1: msgs("paths", httpIngressPath),
}}

var httpIngressPath = &message{"HTTPIngressPath", map[int]field{
	1: str("path"),
	2: msg("backend", ingressBackend),
	3: strPtr("pathType"),
}}

var ingressBackend = &message{"IngressBackend", map[int]field{
	3: msgPtr("resource", typedLocalObjectReference),
	4: msgPtr("service", ingressServiceBackend),
}}

var ingressServiceBackend = &message{"IngressServiceBackend", map[int]field{
	1: str("name"),
	2: msg("port", serviceBackendPort),
}}

var serviceBackendPort = &message{"ServiceBackendPort", map[int]field{
	1: str("name"),
	2: integer("number"),
}}

var typedLocalObjectReference = &message{"TypedLocalObjectReference", map[int]field{
	1: strPtr("apiGroup"),
	2: str("kind"),
	3: str("name"),
}}

var ingressClass = &message{"IngressClass", map[int]field{
	1: msg("metadata", objectMeta),
	2: msg("spec", ingressClassSpec),
}}

var ingressClassSpec = &message{"IngressClassSpec", map[int]field{
	1: str("controller"),
	2: msgPtr("parameters", ingressClassParametersReference),
}}

var ingressClassParametersReference = &message{"IngressClassParametersReference", map[int]field{
	1: strPtr("apiGroup"),
	2: str("kind"),
	3: str("name"),
	4: strPtr("scope"),
	5: strPtr("namespace"),
}}

package crd

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	Group   = "cloudflare.onit.systems"
	Version = "v1alpha1"
)

var (
	SchemeGroupVersion = schema.GroupVersion{Group: Group, Version: Version}
	SchemeBuilder      = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme        = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion,
		&TunnelIngress{},
		&TunnelIngressList{},
	)
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}

// TunnelIngress defines a service to expose via Cloudflare Tunnel
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type TunnelIngress struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              TunnelIngressSpec   `json:"spec"`
	Status            TunnelIngressStatus `json:"status,omitempty"`
}

type TunnelIngressSpec struct {
	// Hostname is the public domain (e.g., argocd.onit.systems)
	Hostname string `json:"hostname"`
	// Service is the backend URL (e.g., http://argocd-server.argocd.svc.cluster.local:80)
	Service string `json:"service"`
	// Access configures Cloudflare Access protection
	Access *AccessSpec `json:"access,omitempty"`
	// DNS configures the DNS record (defaults to proxied CNAME)
	DNS *DNSSpec `json:"dns,omitempty"`
}

type AccessSpec struct {
	// Enabled creates a Cloudflare Access application for this hostname
	Enabled bool `json:"enabled"`
	// BypassPublicIP automatically adds the cluster's public IP to the bypass policy
	BypassPublicIP bool `json:"bypassPublicIP"`
	// AdditionalBypassIPs are extra IPs to add to the bypass policy
	AdditionalBypassIPs []string `json:"additionalBypassIPs,omitempty"`
}

type DNSSpec struct {
	// Proxied determines if traffic goes through Cloudflare (default true)
	Proxied *bool `json:"proxied,omitempty"`
}

type TunnelIngressStatus struct {
	// Ready indicates all resources are synced
	Ready bool `json:"ready"`
	// TunnelConfigured indicates the tunnel ingress rule is set
	TunnelConfigured bool `json:"tunnelConfigured"`
	// DNSConfigured indicates the DNS record exists
	DNSConfigured bool `json:"dnsConfigured"`
	// AccessConfigured indicates Access policy is set
	AccessConfigured bool `json:"accessConfigured"`
	// PublicIP is the detected cluster public IP
	PublicIP string `json:"publicIP,omitempty"`
	// LastSyncTime is when the last reconciliation completed
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`
	// Message contains status details
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
type TunnelIngressList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TunnelIngress `json:"items"`
}

func (in *TunnelIngress) DeepCopyObject() runtime.Object {
	out := new(TunnelIngress)
	in.DeepCopyInto(out)
	return out
}

func (in *TunnelIngress) DeepCopyInto(out *TunnelIngress) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	if in.Spec.Access != nil {
		out.Spec.Access = new(AccessSpec)
		*out.Spec.Access = *in.Spec.Access
		if in.Spec.Access.AdditionalBypassIPs != nil {
			out.Spec.Access.AdditionalBypassIPs = make([]string, len(in.Spec.Access.AdditionalBypassIPs))
			copy(out.Spec.Access.AdditionalBypassIPs, in.Spec.Access.AdditionalBypassIPs)
		}
	}
	if in.Spec.DNS != nil {
		out.Spec.DNS = new(DNSSpec)
		*out.Spec.DNS = *in.Spec.DNS
	}
	out.Status = in.Status
	if in.Status.LastSyncTime != nil {
		out.Status.LastSyncTime = in.Status.LastSyncTime.DeepCopy()
	}
}

func (in *TunnelIngressList) DeepCopyObject() runtime.Object {
	out := new(TunnelIngressList)
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]TunnelIngress, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
	return out
}

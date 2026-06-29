package fromhub

import (
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	corev1 "k8s.io/api/core/v1"
)

// CreateRequest is the wire format for POST .../hostedclusters.
// It matches the server-side CreateRequest in hcp_proxy.go.
type CreateRequest struct {
	// HostedCluster is required.
	HostedCluster *hypershiftv1beta1.HostedCluster `json:"hostedCluster"`

	// NodePools is the list of NodePools to create (--render may produce more than one).
	NodePools []*hypershiftv1beta1.NodePool `json:"nodePools,omitempty"`

	// Secrets holds every Secret that --render-sensitive outputs:
	// pull-secret, ssh-key, and any cloud-platform credential secrets.
	// Each Secret is created on the spoke before the HostedCluster.
	Secrets []corev1.Secret `json:"secrets,omitempty"`
}

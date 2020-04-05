// Copyright (c) 2016-2019 Tigera, Inc. All rights reserved.

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package conversion

import (
	"fmt"
	"sort"
	"strings"

	log "github.com/sirupsen/logrus"
	kapiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	apiv3 "github.com/projectcalico/libcalico-go/lib/apis/v3"
	"github.com/projectcalico/libcalico-go/lib/backend/model"
	"github.com/projectcalico/libcalico-go/lib/names"
	cnet "github.com/projectcalico/libcalico-go/lib/net"
	"github.com/projectcalico/libcalico-go/lib/numorstring"
)

var (
	protoTCP = kapiv1.ProtocolTCP
)

type selectorType int8

const (
	SelectorNamespace selectorType = iota
	SelectorPod
)

type Converter interface {
	WorkloadEndpointConverter
	ParseWorkloadEndpointName(workloadName string) (names.WorkloadEndpointIdentifiers, error)
	NamespaceToProfile(ns *kapiv1.Namespace) (*model.KVPair, error)
	IsValidCalicoWorkloadEndpoint(pod *kapiv1.Pod) bool
	IsReadyCalicoPod(pod *kapiv1.Pod) bool
	IsScheduled(pod *kapiv1.Pod) bool
	IsHostNetworked(pod *kapiv1.Pod) bool
	HasIPAddress(pod *kapiv1.Pod) bool
	StagedKubernetesNetworkPolicyToStagedName(stagedK8sName string) string
	K8sNetworkPolicyToCalico(np *networkingv1.NetworkPolicy) (*model.KVPair, error)
	ProfileNameToNamespace(profileName string) (string, error)
	JoinNetworkPolicyRevisions(crdNPRev, k8sNPRev string) string
	SplitNetworkPolicyRevision(rev string) (crdNPRev string, k8sNPRev string, err error)
	ServiceAccountToProfile(sa *kapiv1.ServiceAccount) (*model.KVPair, error)
	ProfileNameToServiceAccount(profileName string) (ns, sa string, err error)
	JoinProfileRevisions(nsRev, saRev string) string
	SplitProfileRevision(rev string) (nsRev string, saRev string, err error)
}

type converter struct {
	WorkloadEndpointConverter
}

func NewConverter() Converter {
	return &converter{
		WorkloadEndpointConverter: NewWorkloadEndpointConverter(),
	}
}

// ParseWorkloadName extracts the Node name, Orchestrator, Pod name and endpoint from the
// given WorkloadEndpoint name.
// The expected format for k8s is <node>-k8s-<pod>-<endpoint>
func (c converter) ParseWorkloadEndpointName(workloadName string) (names.WorkloadEndpointIdentifiers, error) {
	return names.ParseWorkloadEndpointName(workloadName)
}

// NamespaceToProfile converts a Namespace to a Calico Profile.  The Profile stores
// labels from the Namespace which are inherited by the WorkloadEndpoints within
// the Profile. This Profile also has the default ingress and egress rules, which are both 'allow'.
func (c converter) NamespaceToProfile(ns *kapiv1.Namespace) (*model.KVPair, error) {
	// Generate the labels to apply to the profile, using a special prefix
	// to indicate that these are the labels from the parent Kubernetes Namespace.
	labels := map[string]string{}
	for k, v := range ns.Labels {
		labels[NamespaceLabelPrefix+k] = v
	}

	// Add a label for the namespace's name. This allows exact namespace matching
	// based on name within the namespaceSelector.
	labels[NamespaceLabelPrefix+NameLabel] = ns.Name

	// Create the profile object.
	name := NamespaceProfileNamePrefix + ns.Name
	profile := apiv3.NewProfile()
	profile.ObjectMeta = metav1.ObjectMeta{
		Name:              name,
		CreationTimestamp: ns.CreationTimestamp,
		UID:               ns.UID,
	}
	profile.Spec = apiv3.ProfileSpec{
		Ingress:       []apiv3.Rule{{Action: apiv3.Allow}},
		Egress:        []apiv3.Rule{{Action: apiv3.Allow}},
		LabelsToApply: labels,
	}

	// Embed the profile in a KVPair.
	kvp := model.KVPair{
		Key: model.ResourceKey{
			Name: name,
			Kind: apiv3.KindProfile,
		},
		Value:    profile,
		Revision: c.JoinProfileRevisions(ns.ResourceVersion, ""),
	}
	return &kvp, nil
}

// IsValidCalicoWorkloadEndpoint returns true if the pod should be shown as a workloadEndpoint
// in the Calico API and false otherwise.  Note: since we completely ignore notifications for
// invalid Pods, it is important that pods can only transition from not-valid to valid and not
// the other way.  If they transition from valid to invalid, we'll fail to emit a deletion
// event in the watcher.
func (c converter) IsValidCalicoWorkloadEndpoint(pod *kapiv1.Pod) bool {
	if c.IsHostNetworked(pod) {
		log.WithField("pod", pod.Name).Debug("Pod is host networked.")
		return false
	} else if !c.IsScheduled(pod) {
		log.WithField("pod", pod.Name).Debug("Pod is not scheduled.")
		return false
	}
	return true
}

// IsReadyCalicoPod returns true if the pod is a valid Calico WorkloadEndpoint and has
// an IP address assigned (i.e. it's ready for Calico networking).
func (c converter) IsReadyCalicoPod(pod *kapiv1.Pod) bool {
	if !c.IsValidCalicoWorkloadEndpoint(pod) {
		return false
	} else if !c.HasIPAddress(pod) {
		log.WithField("pod", pod.Name).Debug("Pod does not have an IP address.")
		return false
	}
	return true
}

const (
	// Completed is documented but doesn't seem to be in the API, it should be safe to include.
	// Maybe it's in an older version of the API?
	podCompleted kapiv1.PodPhase = "Completed"
)

func IsFinished(pod *kapiv1.Pod) bool {
	switch pod.Status.Phase {
	case kapiv1.PodFailed, kapiv1.PodSucceeded, podCompleted:
		return true
	}
	return false
}

func (c converter) IsScheduled(pod *kapiv1.Pod) bool {
	return pod.Spec.NodeName != ""
}

func (c converter) IsHostNetworked(pod *kapiv1.Pod) bool {
	return pod.Spec.HostNetwork
}

func (c converter) HasIPAddress(pod *kapiv1.Pod) bool {
	return pod.Status.PodIP != "" || pod.Annotations[AnnotationPodIP] != ""
	// Note: we don't need to check PodIPs and AnnotationPodIPs here, because those cannot be
	// non-empty if the corresponding singular field is empty.
}

// getPodIPs extracts the IP addresses from a Kubernetes Pod.  We support a single IPv4 address
// and/or a single IPv6.  getPodIPs loads the IPs either from the PodIPs and PodIP field, if
// present, or the calico podIP annotation.
func getPodIPs(pod *kapiv1.Pod) ([]*cnet.IPNet, error) {
	var podIPs []string
	if ips := pod.Status.PodIPs; len(ips) != 0 {
		log.WithField("ips", ips).Debug("PodIPs field filled in")
		for _, ip := range ips {
			podIPs = append(podIPs, ip.IP)
		}
	} else if ip := pod.Status.PodIP; ip != "" {
		log.WithField("ip", ip).Debug("PodIP field filled in")
		podIPs = append(podIPs, ip)
	} else if ips := pod.Annotations[AnnotationPodIPs]; ips != "" {
		log.WithField("ips", ips).Debug("No PodStatus IPs, use Calico plural annotation")
		podIPs = append(podIPs, strings.Split(ips, ",")...)
	} else if ip := pod.Annotations[AnnotationPodIP]; ip != "" {
		log.WithField("ip", ip).Debug("No PodStatus IPs, use Calico singular annotation")
		podIPs = append(podIPs, ip)
	} else {
		log.Debug("Pod has no IP")
		return nil, nil
	}
	var podIPNets []*cnet.IPNet
	for _, ip := range podIPs {
		_, ipNet, err := cnet.ParseCIDROrIP(ip)
		if err != nil {
			log.WithFields(log.Fields{"ip": ip, "pod": pod.Name}).WithError(err).Error("Failed to parse pod IP")
			return nil, err
		}
		podIPNets = append(podIPNets, ipNet)
	}
	return podIPNets, nil
}

// StagedKubernetesNetworkPolicyToStagedName converts a StagedKubernetesNetworkPolicy name into a StagedNetworkPolicy name
func (c converter) StagedKubernetesNetworkPolicyToStagedName(stagedK8sName string) string {
	return fmt.Sprintf(K8sNetworkPolicyNamePrefix + stagedK8sName)
}

// K8sNetworkPolicyToCalico converts a k8s NetworkPolicy to a model.KVPair.
func (c converter) K8sNetworkPolicyToCalico(np *networkingv1.NetworkPolicy) (*model.KVPair, error) {
	// Pull out important fields.
	policyName := fmt.Sprintf(K8sNetworkPolicyNamePrefix + np.Name)

	// We insert all the NetworkPolicy Policies at order 1000.0 after conversion.
	// This order might change in future.
	order := float64(1000.0)

	// Generate the ingress rules list.
	var ingressRules []apiv3.Rule
	for _, r := range np.Spec.Ingress {
		rules, err := c.k8sRuleToCalico(r.From, r.Ports, np.Namespace, true)
		if err != nil {
			log.WithError(err).Warn("dropping k8s rule that couldn't be converted.")
		} else {
			ingressRules = append(ingressRules, rules...)
		}
	}

	// Generate the egress rules list.
	var egressRules []apiv3.Rule
	for _, r := range np.Spec.Egress {
		rules, err := c.k8sRuleToCalico(r.To, r.Ports, np.Namespace, false)
		if err != nil {
			log.WithError(err).Warn("dropping k8s rule that couldn't be converted")
		} else {
			egressRules = append(egressRules, rules...)
		}
	}

	// Calculate Types setting.
	ingress := false
	egress := false
	for _, policyType := range np.Spec.PolicyTypes {
		switch policyType {
		case networkingv1.PolicyTypeIngress:
			ingress = true
		case networkingv1.PolicyTypeEgress:
			egress = true
		}
	}
	types := []apiv3.PolicyType{}
	if ingress {
		types = append(types, apiv3.PolicyTypeIngress)
	}
	if egress {
		types = append(types, apiv3.PolicyTypeEgress)
	} else if len(egressRules) > 0 {
		// Egress was introduced at the same time as policyTypes.  It shouldn't be possible to
		// receive a NetworkPolicy with an egress rule but without "Egress" specified in its types,
		// but we'll warn about it anyway.
		log.Warn("K8s PolicyTypes don't include 'egress', but NetworkPolicy has egress rules.")
	}

	// If no types were specified in the policy, then we're running on a cluster that doesn't
	// include support for that field in the API.  In that case, the correct behavior is for the policy
	// to apply to only ingress traffic.
	if len(types) == 0 {
		types = append(types, apiv3.PolicyTypeIngress)
	}

	// Create the NetworkPolicy.
	policy := apiv3.NewNetworkPolicy()
	policy.ObjectMeta = metav1.ObjectMeta{
		Name:              policyName,
		Namespace:         np.Namespace,
		CreationTimestamp: np.CreationTimestamp,
		UID:               np.UID,
		ResourceVersion:   np.ResourceVersion,
	}
	policy.Spec = apiv3.NetworkPolicySpec{
		Order:    &order,
		Selector: c.k8sSelectorToCalico(&np.Spec.PodSelector, SelectorPod),
		Ingress:  ingressRules,
		Egress:   egressRules,
		Types:    types,
	}

	// Build and return the KVPair.
	return &model.KVPair{
		Key: model.ResourceKey{
			Name:      policyName,
			Namespace: np.Namespace,
			Kind:      apiv3.KindNetworkPolicy,
		},
		Value:    policy,
		Revision: np.ResourceVersion,
	}, nil
}

// k8sSelectorToCalico takes a namespaced k8s label selector and returns the Calico
// equivalent.
func (c converter) k8sSelectorToCalico(s *metav1.LabelSelector, selectorType selectorType) string {
	// Only prefix pod selectors - this won't work for namespace selectors.
	selectors := []string{}
	if selectorType == SelectorPod {
		selectors = append(selectors, fmt.Sprintf("%s == 'k8s'", apiv3.LabelOrchestrator))
	}

	if s == nil {
		return strings.Join(selectors, " && ")
	}

	// For namespace selectors, if they are present but have no terms, it means "select all
	// namespaces". We use empty string to represent the nil namespace selector, so use all() to
	// represent all namespaces.
	if selectorType == SelectorNamespace && len(s.MatchLabels) == 0 && len(s.MatchExpressions) == 0 {
		return "all()"
	}

	// matchLabels is a map key => value, it means match if (label[key] ==
	// value) for all keys.
	keys := make([]string, 0, len(s.MatchLabels))
	for k := range s.MatchLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := s.MatchLabels[k]
		selectors = append(selectors, fmt.Sprintf("%s == '%s'", k, v))
	}

	// matchExpressions is a list of in/notin/exists/doesnotexist tests.
	for _, e := range s.MatchExpressions {
		valueList := strings.Join(e.Values, "', '")

		// Each selector is formatted differently based on the operator.
		switch e.Operator {
		case metav1.LabelSelectorOpIn:
			selectors = append(selectors, fmt.Sprintf("%s in { '%s' }", e.Key, valueList))
		case metav1.LabelSelectorOpNotIn:
			selectors = append(selectors, fmt.Sprintf("%s not in { '%s' }", e.Key, valueList))
		case metav1.LabelSelectorOpExists:
			selectors = append(selectors, fmt.Sprintf("has(%s)", e.Key))
		case metav1.LabelSelectorOpDoesNotExist:
			selectors = append(selectors, fmt.Sprintf("! has(%s)", e.Key))
		}
	}

	return strings.Join(selectors, " && ")
}

func (c converter) k8sRuleToCalico(rPeers []networkingv1.NetworkPolicyPeer, rPorts []networkingv1.NetworkPolicyPort, ns string, ingress bool) ([]apiv3.Rule, error) {
	rules := []apiv3.Rule{}
	peers := []*networkingv1.NetworkPolicyPeer{}
	ports := []*networkingv1.NetworkPolicyPort{}

	// Built up a list of the sources and a list of the destinations.
	for _, f := range rPeers {
		// We need to add a copy of the peer so all the rules don't
		// point to the same location.
		peers = append(peers, &networkingv1.NetworkPolicyPeer{
			NamespaceSelector: f.NamespaceSelector,
			PodSelector:       f.PodSelector,
			IPBlock:           f.IPBlock,
		})
	}
	for _, p := range rPorts {
		// We need to add a copy of the port so all the rules don't
		// point to the same location.
		port := networkingv1.NetworkPolicyPort{}
		if p.Port != nil {
			portval := intstr.FromString(p.Port.String())
			port.Port = &portval

			// TCP is the implicit default (as per the definition of NetworkPolicyPort).
			// Make the default explicit here because our data-model always requires
			// the protocol to be specified if we're doing a port match.
			port.Protocol = &protoTCP
		}
		if p.Protocol != nil {
			protval := kapiv1.Protocol(fmt.Sprintf("%s", *p.Protocol))
			port.Protocol = &protval
		}
		ports = append(ports, &port)
	}

	// If there no peers, or no ports, represent that as nil.
	if len(peers) == 0 {
		peers = []*networkingv1.NetworkPolicyPeer{nil}
	}
	if len(ports) == 0 {
		ports = []*networkingv1.NetworkPolicyPort{nil}
	}

	protocolPorts := map[string][]numorstring.Port{}

	for _, port := range ports {
		protocol, calicoPorts, err := c.k8sPortToCalicoFields(port)
		if err != nil {
			return nil, fmt.Errorf("failed to parse k8s port: %s", err)
		}

		// These are either both present or both nil
		if protocol == nil && calicoPorts == nil {
			// If nil, no ports were specified, or an empty port struct was provided, which we translate to allowing all.
			// We want to use a nil protocol and a nil list of ports, which will allow any destination (for ingress).
			// Given we're gonna allow all, we may as well break here and keep only this rule
			protocolPorts = map[string][]numorstring.Port{"": nil}
			break
		}

		pStr := protocol.String()
		protocolPorts[pStr] = append(protocolPorts[pStr], calicoPorts...)
	}

	protocols := make([]string, 0, len(protocolPorts))
	for k := range protocolPorts {
		protocols = append(protocols, k)
	}
	// Ensure deterministic output
	sort.Strings(protocols)

	// Combine destinations with sources to generate rules. We generate one rule per protocol,
	// with each rule containing all the allowed ports.
	for _, protocolStr := range protocols {
		calicoPorts := protocolPorts[protocolStr]

		var protocol *numorstring.Protocol
		if protocolStr != "" {
			p := numorstring.ProtocolFromString(protocolStr)
			protocol = &p
		}

		for _, peer := range peers {
			selector, nsSelector, nets, notNets := c.k8sPeerToCalicoFields(peer, ns)
			if ingress {
				// Build inbound rule and append to list.
				rules = append(rules, apiv3.Rule{
					Action:   "Allow",
					Protocol: protocol,
					Source: apiv3.EntityRule{
						Selector:          selector,
						NamespaceSelector: nsSelector,
						Nets:              nets,
						NotNets:           notNets,
					},
					Destination: apiv3.EntityRule{
						Ports: calicoPorts,
					},
				})
			} else {
				// Build outbound rule and append to list.
				rules = append(rules, apiv3.Rule{
					Action:   "Allow",
					Protocol: protocol,
					Destination: apiv3.EntityRule{
						Ports:             calicoPorts,
						Selector:          selector,
						NamespaceSelector: nsSelector,
						Nets:              nets,
						NotNets:           notNets,
					},
				})
			}
		}
	}
	return rules, nil
}

func (c converter) k8sPortToCalicoFields(port *networkingv1.NetworkPolicyPort) (protocol *numorstring.Protocol, dstPorts []numorstring.Port, err error) {
	// If no port info, return zero values for all fields (protocol, dstPorts).
	if port == nil {
		return
	}
	// Port information available.
	dstPorts, err = c.k8sPortToCalico(*port)
	if err != nil {
		return
	}
	protocol = c.k8sProtocolToCalico(port.Protocol)
	return
}

func (c converter) k8sProtocolToCalico(protocol *kapiv1.Protocol) *numorstring.Protocol {
	if protocol != nil {
		p := numorstring.ProtocolFromString(string(*protocol))
		return &p
	}
	return nil
}

func (c converter) k8sPeerToCalicoFields(peer *networkingv1.NetworkPolicyPeer, ns string) (selector, nsSelector string, nets []string, notNets []string) {
	// If no peer, return zero values for all fields (selector, nets and !nets).
	if peer == nil {
		return
	}
	// Peer information available.
	// Determine the source selector for the rule.
	if peer.IPBlock != nil {
		// Convert the CIDR to include.
		_, ipNet, err := cnet.ParseCIDR(peer.IPBlock.CIDR)
		if err != nil {
			log.WithField("cidr", peer.IPBlock.CIDR).WithError(err).Error("Failed to parse CIDR")
			return
		}
		nets = []string{ipNet.String()}

		// Convert the CIDRs to exclude.
		for _, exception := range peer.IPBlock.Except {
			_, ipNet, err = cnet.ParseCIDR(exception)
			if err != nil {
				log.WithField("cidr", exception).WithError(err).Error("Failed to parse CIDR")
				return
			}
			notNets = append(notNets, ipNet.String())
		}
		// If IPBlock is set, then PodSelector and NamespaceSelector cannot be.
		return
	}

	// IPBlock is not set to get here.
	// Note that k8sSelectorToCalico() accepts nil values of the selector.
	selector = c.k8sSelectorToCalico(peer.PodSelector, SelectorPod)
	nsSelector = c.k8sSelectorToCalico(peer.NamespaceSelector, SelectorNamespace)
	return
}

func (c converter) k8sPortToCalico(port networkingv1.NetworkPolicyPort) ([]numorstring.Port, error) {
	var portList []numorstring.Port
	if port.Port != nil {
		p, err := numorstring.PortFromString(port.Port.String())
		if err != nil {
			return nil, fmt.Errorf("invalid port %+v: %s", port.Port, err)
		}
		return append(portList, p), nil
	}

	// No ports - return empty list.
	return portList, nil
}

// ProfileNameToNamespace extracts the Namespace name from the given Profile name.
func (c converter) ProfileNameToNamespace(profileName string) (string, error) {
	// Profile objects backed by Namespaces have form "kns.<ns_name>"
	if !strings.HasPrefix(profileName, NamespaceProfileNamePrefix) {
		// This is not backed by a Kubernetes Namespace.
		return "", fmt.Errorf("Profile %s not backed by a Namespace", profileName)
	}

	return strings.TrimPrefix(profileName, NamespaceProfileNamePrefix), nil
}

// JoinNetworkPolicyRevisions constructs the revision from the individual CRD and K8s NetworkPolicy
// revisions.
func (c converter) JoinNetworkPolicyRevisions(crdNPRev, k8sNPRev string) string {
	return crdNPRev + "/" + k8sNPRev
}

// SplitNetworkPolicyRevision extracts the CRD and K8s NetworkPolicy revisions from the combined
// revision returned on the KDD NetworkPolicy client.
func (c converter) SplitNetworkPolicyRevision(rev string) (crdNPRev string, k8sNPRev string, err error) {
	if rev == "" {
		return
	}

	revs := strings.Split(rev, "/")
	if len(revs) != 2 {
		err = fmt.Errorf("ResourceVersion is not valid: %s", rev)
		return
	}

	crdNPRev = revs[0]
	k8sNPRev = revs[1]
	return
}

// serviceAccountNameToProfileName creates a profile name that is a join
// of 'ksa.' + namespace + "." + serviceaccount name.
func serviceAccountNameToProfileName(sa, namespace string) string {
	// Need to incorporate the namespace into the name of the sa based profile
	// to make them globally unique
	if namespace == "" {
		namespace = "default"
	}
	return ServiceAccountProfileNamePrefix + namespace + "." + sa
}

// ServiceAccountToProfile converts a ServiceAccount to a Calico Profile.  The Profile stores
// labels from the ServiceAccount which are inherited by the WorkloadEndpoints within
// the Profile.
func (c converter) ServiceAccountToProfile(sa *kapiv1.ServiceAccount) (*model.KVPair, error) {
	// Generate the labels to apply to the profile, using a special prefix
	// to indicate that these are the labels from the parent Kubernetes ServiceAccount.
	labels := map[string]string{}
	for k, v := range sa.ObjectMeta.Labels {
		labels[ServiceAccountLabelPrefix+k] = v
	}

	// Add a label for the serviceaccount's name. This allows exact namespace matching
	// based on name within the serviceAccountSelector.
	labels[ServiceAccountLabelPrefix+NameLabel] = sa.Name

	name := serviceAccountNameToProfileName(sa.Name, sa.Namespace)
	profile := apiv3.NewProfile()
	profile.ObjectMeta = metav1.ObjectMeta{
		Name:              name,
		CreationTimestamp: sa.CreationTimestamp,
		UID:               sa.UID,
	}
	profile.Spec.LabelsToApply = labels

	// Embed the profile in a KVPair.
	kvp := model.KVPair{
		Key: model.ResourceKey{
			Name: name,
			Kind: apiv3.KindProfile,
		},
		Value:    profile,
		Revision: c.JoinProfileRevisions("", sa.ResourceVersion),
	}
	return &kvp, nil
}

// ProfileNameToServiceAccount extracts the ServiceAccount name from the given Profile name.
func (c converter) ProfileNameToServiceAccount(profileName string) (ns, sa string, err error) {

	// Profile objects backed by ServiceAccounts have form "ksa.<namespace>.<sa_name>"
	if !strings.HasPrefix(profileName, ServiceAccountProfileNamePrefix) {
		// This is not backed by a Kubernetes ServiceAccount.
		err = fmt.Errorf("Profile %s not backed by a ServiceAccount", profileName)
		return
	}

	names := strings.SplitN(profileName, ".", 3)
	if len(names) != 3 {
		err = fmt.Errorf("Profile %s is not formatted correctly", profileName)
		return
	}

	ns = names[1]
	sa = names[2]
	return
}

// JoinProfileRevisions constructs the revision from the individual namespace and serviceaccount
// revisions.
// This is conditional on the feature flag for serviceaccount set or not.
func (c converter) JoinProfileRevisions(nsRev, saRev string) string {
	return nsRev + "/" + saRev
}

// SplitProfileRevision extracts the namespace and serviceaccount revisions from the combined
// revision returned on the KDD service account based profile.
// This is conditional on the feature flag for serviceaccount set or not.
func (c converter) SplitProfileRevision(rev string) (nsRev string, saRev string, err error) {
	if rev == "" {
		return
	}

	revs := strings.Split(rev, "/")
	if len(revs) != 2 {
		err = fmt.Errorf("ResourceVersion is not valid: %s", rev)
		return
	}

	nsRev = revs[0]
	saRev = revs[1]
	return
}

func stringsToIPNets(ipStrings []string) ([]*cnet.IPNet, error) {
	var podIPNets []*cnet.IPNet
	for _, ip := range ipStrings {
		_, ipNet, err := cnet.ParseCIDROrIP(ip)
		if err != nil {
			return nil, err
		}
		podIPNets = append(podIPNets, ipNet)
	}
	return podIPNets, nil
}

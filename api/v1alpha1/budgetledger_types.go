/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// BudgetLedgerSpec is one account's durable usage: the gateway reserves and
// settles against it, and the remote executor initializes and seals the
// accounts of its managed sessions. It is state, not desired state, held in
// spec like a coordination Lease, so every write is one compare-and-swap
// update on the whole object (ADR 0045).
//
// +kubebuilder:validation:XValidation:rule="self.account == oldSelf.account",message="spec.account is immutable"
type BudgetLedgerSpec struct {
	// Account is the account key: on Kubernetes, the run UID and capability
	// joined by "/". The object name is derived from it.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Account string `json:"account"`

	// RunUID is the run whose usage this account holds, empty for an account
	// key without one. It is also the sproozi.com/run-uid label.
	// +kubebuilder:validation:MaxLength=63
	// +optional
	RunUID string `json:"runUID,omitempty"`

	// Capability is the account's capability, such as model.inference.
	// +kubebuilder:validation:MaxLength=256
	// +optional
	Capability string `json:"capability,omitempty"`

	// State is the ledger's encoded counters, reservations, leases and seal.
	// Only the gateway and the remote executor interpret it.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1048576
	State string `json:"state"`

	// UnitsUsed copies the settled units from State, for display.
	// +kubebuilder:validation:Minimum=0
	// +optional
	UnitsUsed int64 `json:"unitsUsed,omitempty"`

	// CostMicros copies the settled cost from State, for display.
	// +kubebuilder:validation:Minimum=0
	// +optional
	CostMicros int64 `json:"costMicros,omitempty"`

	// Finalized is true once the account is sealed: its usage can no longer
	// change.
	// +optional
	Finalized bool `json:"finalized,omitempty"`

	// WrittenAt is when the ledger was last written. Retention counts from it.
	WrittenAt metav1.Time `json:"writtenAt"`
}

// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Run",type=string,JSONPath=`.spec.runUID`
// +kubebuilder:printcolumn:name="Capability",type=string,JSONPath=`.spec.capability`
// +kubebuilder:printcolumn:name="Units",type=integer,JSONPath=`.spec.unitsUsed`
// +kubebuilder:printcolumn:name="Finalized",type=boolean,JSONPath=`.spec.finalized`
// +kubebuilder:printcolumn:name="Written",type=date,JSONPath=`.spec.writtenAt`

// BudgetLedger is one run account's durable usage, written by the gateway and,
// for managed sessions, the remote executor. It is a separate resource so that
// write access to ledgers grants no write access to ConfigMaps, which hold the
// gateway's own configuration.
type BudgetLedger struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec holds the account's usage
	// +required
	Spec BudgetLedgerSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// BudgetLedgerList contains a list of BudgetLedger
type BudgetLedgerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []BudgetLedger `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &BudgetLedger{}, &BudgetLedgerList{})
		return nil
	})
}

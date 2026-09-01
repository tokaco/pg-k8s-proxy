package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// BackendType selects which backend discriminator field is populated.
// +kubebuilder:validation:Enum=Service;Address
type BackendType string

const (
	// BackendTypeService routes to a Kubernetes Service by its cluster DNS name.
	BackendTypeService BackendType = "Service"
	// BackendTypeAddress routes to a host/port that lives outside the cluster.
	BackendTypeAddress BackendType = "Address"
)

// BackendTLSMode controls how the proxy connects to the backend.
// +kubebuilder:validation:Enum=Disable;Require;VerifyCA;VerifyFull
type BackendTLSMode string

const (
	// BackendTLSDisable connects in plaintext.
	BackendTLSDisable BackendTLSMode = "Disable"
	// BackendTLSRequire negotiates TLS but performs no certificate verification.
	BackendTLSRequire BackendTLSMode = "Require"
	// BackendTLSVerifyCA verifies the backend certificate chain but not its hostname.
	BackendTLSVerifyCA BackendTLSMode = "VerifyCA"
	// BackendTLSVerifyFull verifies both the certificate chain and the hostname.
	BackendTLSVerifyFull BackendTLSMode = "VerifyFull"
)

// ServiceBackend addresses a PostgreSQL instance fronted by a Kubernetes Service.
type ServiceBackend struct {
	// Name of the Service.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Namespace of the Service. Defaults to the namespace of the PostgresRoute.
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Port is the Service port to connect to, either a port number or a named
	// port declared on the Service. Defaults to 5432.
	// +kubebuilder:default="5432"
	// +optional
	Port intstr.IntOrString `json:"port,omitempty"`
}

// AddressBackend addresses a PostgreSQL instance by host and port. The host is
// resolved by the proxy at connection time, so DNS names outside the cluster work.
type AddressBackend struct {
	// Host is a DNS name or IP address.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Host string `json:"host"`

	// Port is the TCP port to connect to.
	// +kubebuilder:default=5432
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`
}

// Backend describes where connections for this route are sent.
// +kubebuilder:validation:XValidation:rule="self.type == 'Service' ? has(self.service) : !has(self.service)",message="service must be set if and only if type is Service"
// +kubebuilder:validation:XValidation:rule="self.type == 'Address' ? has(self.address) : !has(self.address)",message="address must be set if and only if type is Address"
type Backend struct {
	// Type selects which of the fields below is used.
	// +kubebuilder:default=Service
	Type BackendType `json:"type"`

	// Service routes to a Kubernetes Service. Used when type is Service.
	// +optional
	Service *ServiceBackend `json:"service,omitempty"`

	// Address routes to an arbitrary host/port. Used when type is Address.
	// +optional
	Address *AddressBackend `json:"address,omitempty"`
}

// BackendTLS configures the proxy-to-backend leg of the connection.
type BackendTLS struct {
	// Mode selects the TLS behaviour towards the backend.
	// +kubebuilder:default=Disable
	// +optional
	Mode BackendTLSMode `json:"mode,omitempty"`

	// CASecretRef names a Secret in the route's namespace holding the CA bundle
	// used to verify the backend certificate. Required for VerifyCA and VerifyFull.
	// +optional
	CASecretRef *SecretKeyReference `json:"caSecretRef,omitempty"`

	// ServerName overrides the name verified against the backend certificate.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	ServerName string `json:"serverName,omitempty"`
}

// SecretKeyReference points at a single key inside a Secret.
type SecretKeyReference struct {
	// Name of the Secret.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Key within the Secret.
	// +kubebuilder:default="ca.crt"
	// +kubebuilder:validation:MinLength=1
	// +optional
	Key string `json:"key,omitempty"`
}

// PostgresRouteSpec defines a database-name to backend mapping.
type PostgresRouteSpec struct {
	// Database is the name clients pass in the PostgreSQL startup message.
	// Defaults to the name of the PostgresRoute object.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-zA-Z_][a-zA-Z0-9_$-]*$`
	// +optional
	Database string `json:"database,omitempty"`

	// TargetDatabase rewrites the database name sent on to the backend. Use it
	// when the externally visible name differs from the name on the instance.
	// Defaults to the effective value of Database.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-zA-Z_][a-zA-Z0-9_$-]*$`
	// +optional
	TargetDatabase string `json:"targetDatabase,omitempty"`

	// Backend describes where matching connections are sent.
	Backend Backend `json:"backend"`

	// TLS configures the proxy-to-backend connection.
	// +optional
	TLS *BackendTLS `json:"tls,omitempty"`

	// Priority breaks ties when several routes claim the same database name.
	// The highest priority wins; equal priorities fall back to the oldest object.
	// +kubebuilder:default=0
	// +optional
	Priority int32 `json:"priority,omitempty"`
}

// Condition types reported on a PostgresRoute.
const (
	// ConditionAccepted is true when this route owns its database name. It is
	// false when another route claimed the same name first.
	ConditionAccepted = "Accepted"
	// ConditionResolved is true when the backend address could be resolved.
	ConditionResolved = "Resolved"
	// ConditionProgrammed is true when the route is serving traffic.
	ConditionProgrammed = "Programmed"
)

// Condition reasons reported on a PostgresRoute.
const (
	ReasonAccepted         = "Accepted"
	ReasonDatabaseConflict = "DatabaseNameConflict"
	ReasonResolved         = "Resolved"
	ReasonBackendNotFound  = "BackendNotFound"
	ReasonPortNotFound     = "PortNotFound"
	ReasonInvalidBackend   = "InvalidBackend"
	ReasonProgrammed       = "Programmed"
	ReasonNotProgrammed    = "NotProgrammed"
)

// PostgresRouteStatus reports the observed state of a PostgresRoute.
type PostgresRouteStatus struct {
	// ObservedGeneration is the spec generation the controller last acted on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Database is the effective database name this route claims.
	// +optional
	Database string `json:"database,omitempty"`

	// Endpoint is the resolved backend address in host:port form.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// ConflictingRoute names the route that won the database name, when this
	// route was rejected for a conflict.
	// +optional
	ConflictingRoute string `json:"conflictingRoute,omitempty"`

	// Conditions describe the current state of the route.
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=pgroute;pgr,categories=pgproxy
// +kubebuilder:printcolumn:name="Database",type=string,JSONPath=`.status.database`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=`.status.conditions[?(@.type=="Accepted")].status`
// +kubebuilder:printcolumn:name="Programmed",type=string,JSONPath=`.status.conditions[?(@.type=="Programmed")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PostgresRoute maps a PostgreSQL database name to a backend instance. The
// gateway reads the startup message of every incoming connection and forwards
// it to the backend of the route claiming that database name.
type PostgresRoute struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec PostgresRouteSpec `json:"spec,omitempty"`
	// +optional
	Status PostgresRouteStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PostgresRouteList contains a list of PostgresRoute.
type PostgresRouteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PostgresRoute `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PostgresRoute{}, &PostgresRouteList{})
}

// EffectiveDatabase returns the database name this route claims.
func (r *PostgresRoute) EffectiveDatabase() string {
	if r.Spec.Database != "" {
		return r.Spec.Database
	}
	return r.Name
}

// EffectiveTargetDatabase returns the database name sent on to the backend.
func (r *PostgresRoute) EffectiveTargetDatabase() string {
	if r.Spec.TargetDatabase != "" {
		return r.Spec.TargetDatabase
	}
	return r.EffectiveDatabase()
}

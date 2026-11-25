package ha

// What a joining control plane or witness receives from the leader over the
// join token's TLS connection (design 3.8), and the files the processes use
// to find each other.

const (
	// RoleWorker, RoleControlPlane and RoleWitness name what a join token may
	// join as (nodeapi.JoinRequest.Role).
	RoleWorker       = "worker"
	RoleControlPlane = "control-plane"
	RoleWitness      = "witness"

	// StatusFile is written by a running standby every few seconds
	// (StandbyStatus), for sw ha status on that machine.
	StatusFile = "ha-status.json"
	// WitnessStateFile is the witness's vote record (WitnessState).
	WitnessStateFile = "witness-state.json"
)

// Bundle is the secret material a control plane or witness needs. It is sent
// once, only over the join request authenticated by a single-use join token
// whose role matches, and written with mode 0600.
type Bundle struct {
	// Config is the joiner's ha.json (a control plane) or the witness's
	// configuration (Self is the witness's name, Members the control planes).
	Config Config `json:"config"`
	CACert string `json:"caCert"` // PEM
	// For a control plane: the cluster CA key and the administrator token,
	// so it can serve the API and sign node certificates after it takes over.
	CAKey      string `json:"caKey,omitempty"`
	AdminToken string `json:"adminToken,omitempty"`
	// ServingCert/Key: a certificate for the joiner's own address signed by
	// the cluster CA (the witness's HTTPS listener). A control plane makes
	// its own from CAKey.
	ServingCert string `json:"servingCert,omitempty"`
	ServingKey  string `json:"servingKey,omitempty"`
	// Listen is the witness's HTTPS address (IP:PORT).
	Listen string `json:"listen,omitempty"`
}

// WitnessPort is the witness's default HTTPS port.
const WitnessPort = 6445

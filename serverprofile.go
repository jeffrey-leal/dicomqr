package main

// Transfer syntax preference values for ServerProfile.TransferSyntax.
const (
	tsPrefAny        = ""            // as stored — server decides
	tsPrefExplicitLE = "explicit-le" // uncompressed, Explicit VR Little Endian preferred
	tsPrefImplicitLE = "implicit-le" // uncompressed, Implicit VR Little Endian preferred
)

// ServerProfile holds connection parameters for a remote DICOM server.
type ServerProfile struct {
	Name           string `json:"name"`
	RemoteAETitle  string `json:"remoteAETitle"`
	Host           string `json:"host"`
	Port           int    `json:"port"`
	InfoModel      string `json:"infoModel"`      // "study", "patient", or "patient-study-only"
	RetrieveMethod string `json:"retrieveMethod"` // "MOVE" (default), "GET", or "AUTO"
	ConnectTimeout int    `json:"connectTimeout"` // seconds; 0 means default (10s)

	// TransferSyntax selects the negotiated transfer syntax for retrieves:
	// tsPrefAny (empty), tsPrefExplicitLE, or tsPrefImplicitLE.
	TransferSyntax string `json:"transferSyntax,omitempty"`

	// EnsureUncompressed guarantees files on disk end up in Implicit or
	// Explicit VR Little Endian: existing compressed copies are replaced on
	// re-retrieve, and received compressed files are decompressed locally
	// when a built-in decoder exists for their syntax.
	EnsureUncompressed bool `json:"ensureUncompressed,omitempty"`

	// TransferUncompressed is the deprecated pre-v1.7 flag, migrated to
	// TransferSyntax + EnsureUncompressed by migrateProfile on load.
	TransferUncompressed bool `json:"transferUncompressed,omitempty"`
}

// migrateProfile converts the deprecated TransferUncompressed flag to the
// TransferSyntax/EnsureUncompressed pair. Old behaviour was "negotiate
// uncompressed only", which maps to Explicit VR LE preferred with the on-disk
// guarantee enabled.
func migrateProfile(p *ServerProfile) {
	if p.TransferUncompressed && p.TransferSyntax == tsPrefAny {
		p.TransferSyntax = tsPrefExplicitLE
		p.EnsureUncompressed = true
	}
	p.TransferUncompressed = false
}

// wantsUncompressed reports whether the profile asks for an uncompressed
// transfer syntax during negotiation.
func (p ServerProfile) wantsUncompressed() bool {
	return p.TransferSyntax == tsPrefExplicitLE || p.TransferSyntax == tsPrefImplicitLE
}

// preferredTransferSyntaxes returns the uncompressed syntaxes in the profile's
// preference order, or nil when the profile accepts anything. Used both as the
// C-GET proposal list and as the storage SCP's accepted set.
func (p ServerProfile) preferredTransferSyntaxes() []string {
	switch p.TransferSyntax {
	case tsPrefExplicitLE:
		return []string{tsExplicitVRLE, tsImplicitVRLE}
	case tsPrefImplicitLE:
		return []string{tsImplicitVRLE, tsExplicitVRLE}
	}
	return nil
}

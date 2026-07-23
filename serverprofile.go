package main

// Transfer syntax requirement values for ServerProfile.TransferSyntax.
const (
	tsPrefAny        = ""            // as stored — server decides
	tsPrefExplicitLE = "explicit-le" // uncompressed, Explicit VR Little Endian only
	tsPrefImplicitLE = "implicit-le" // uncompressed, Implicit VR Little Endian only
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

	// TransferSyntax selects the transfer syntax REQUIRED for retrieves:
	// tsPrefAny (empty) accepts whatever the server sends; tsPrefExplicitLE or
	// tsPrefImplicitLE make that single syntax the only one offered in
	// negotiation, so every received file is guaranteed to be in it — a server
	// that cannot transcode fails the retrieve visibly instead of delivering a
	// mixed or compressed study.
	TransferSyntax string `json:"transferSyntax,omitempty"`

	// TransferUncompressed is the deprecated pre-v1.7 flag, migrated to
	// TransferSyntax by migrateProfile on load.
	TransferUncompressed bool `json:"transferUncompressed,omitempty"`
}

// migrateProfile converts the deprecated TransferUncompressed flag to the
// TransferSyntax requirement. Old behaviour was "negotiate uncompressed only",
// which maps to Explicit VR LE. (The v1.7.0 ensureUncompressed JSON field is
// intentionally dropped: strict negotiation replaced the local-decompress
// guarantee, so the flag no longer has meaning.)
func migrateProfile(p *ServerProfile) {
	if p.TransferUncompressed && p.TransferSyntax == tsPrefAny {
		p.TransferSyntax = tsPrefExplicitLE
	}
	p.TransferUncompressed = false
}

// requiredTransferSyntax returns the single transfer syntax UID the profile
// demands for every retrieved file, or "" when the profile accepts anything
// (as stored). This UID is the only syntax offered in negotiation — as the
// C-GET proposal and as the storage SCP's accepted set for C-MOVE deliveries —
// so a retrieve either yields files exclusively in this syntax or fails.
func (p ServerProfile) requiredTransferSyntax() string {
	switch p.TransferSyntax {
	case tsPrefExplicitLE:
		return tsExplicitVRLE
	case tsPrefImplicitLE:
		return tsImplicitVRLE
	}
	return ""
}

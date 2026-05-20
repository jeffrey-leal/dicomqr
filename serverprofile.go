package main

// ServerProfile holds connection parameters for a remote DICOM server.
type ServerProfile struct {
	Name          string `json:"name"`
	RemoteAETitle string `json:"remoteAETitle"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	InfoModel     string `json:"infoModel"` // "study" or "patient"
}

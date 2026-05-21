package fuzze2e

import (
	"context"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"

	"github.com/algm/go-netdicom"
	"github.com/algm/go-netdicom/dimse"
	"github.com/algm/go-netdicom/sopclass"
	"github.com/grailbio/go-dicom"
)

func startServer(faults netdicom.FaultInjector) net.Listener {
	netdicom.SetProviderFaultInjector(faults)
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		log.Panic(err)
	}
	go func() {
		// TODO(saito) test w/ small PDU.
		params := netdicom.ServiceProviderParams{
			CStore: func(
				ctx context.Context,
				connState netdicom.ConnectionState,
				transferSyntaxUID string,
				sopClassUID string,
				sopInstanceUID string,
				dataReader io.Reader,
				dataSize int64) dimse.Status {
				return dimse.Status{Status: dimse.StatusSuccess}
			},
		}

		for {
			conn, err := listener.Accept()
			if err != nil {
				log.Printf("Accept error: %v", err)
				break
			}
			log.Printf("Accepted connection %v", conn)
			netdicom.RunProviderForConn(context.Background(), conn, params)
		}
	}()
	return listener
}

func runClient(serverAddr string, faults netdicom.FaultInjector) error {
	// Find the test data file
	testFile := "../testdata/reportsi.dcm"
	if _, err := os.Stat(testFile); os.IsNotExist(err) {
		// Try alternative path
		testFile = "testdata/reportsi.dcm"
		if _, err := os.Stat(testFile); os.IsNotExist(err) {
			// Try absolute path from current working directory
			wd, _ := os.Getwd()
			testFile = filepath.Join(wd, "..", "testdata", "reportsi.dcm")
			if _, err := os.Stat(testFile); os.IsNotExist(err) {
				return err
			}
		}
	}

	dataset, err := dicom.ReadDataSetFromFile(testFile, dicom.ReadOptions{})
	if err != nil {
		return err
	}
	netdicom.SetUserFaultInjector(faults)
	su, err := netdicom.NewServiceUser(netdicom.ServiceUserParams{SOPClasses: sopclass.StorageClasses})
	if err != nil {
		return err
	}
	su.Connect(serverAddr)
	err = su.CStore(dataset)
	log.Printf("Store done with status: %v", err)
	su.Release()
	return nil
}

func Fuzz(data []byte) int {
	listener := startServer(netdicom.NewFuzzFaultInjector(data))
	defer listener.Close()
	err := runClient(listener.Addr().String(), netdicom.NewFuzzFaultInjector(data))
	if err != nil {
		// Don't panic during fuzzing, just log and continue
		log.Printf("Client error during fuzzing: %v", err)
	}
	return 0
}

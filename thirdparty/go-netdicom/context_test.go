package netdicom

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/algm/go-netdicom/dimse"
	"github.com/algm/go-netdicom/sopclass"
	"github.com/grailbio/go-dicom"
	"github.com/stretchr/testify/require"
)

// Simple C-STORE handler for testing
func testCStoreHandler(
	ctx context.Context,
	connState ConnectionState,
	transferSyntaxUID string,
	sopClassUID string,
	sopInstanceUID string,
	dataReader io.Reader,
	dataSize int64) dimse.Status {

	// Check if context is cancelled
	select {
	case <-ctx.Done():
		log.Printf("C-STORE handler cancelled due to context")
		return dimse.Status{
			Status:       dimse.CStoreCannotUnderstand,
			ErrorComment: "Server shutting down",
		}
	default:
	}

	log.Printf("Processing C-STORE: transferSyntax=%s, sopClass=%s, sopInstance=%s, dataSize=%d bytes",
		transferSyntaxUID, sopClassUID, sopInstanceUID, dataSize)

	return dimse.Success
}

// Helper function to read test DICOM file
func mustReadTestDICOMFile(path string) *dicom.DataSet {
	dataset, err := dicom.ReadDataSetFromFile(path, dicom.ReadOptions{})
	if err != nil {
		log.Panic(err)
	}
	return dataset
}

func TestStoreWithContext(t *testing.T) {
	// Test C-STORE with context cancellation
	dataset := mustReadTestDICOMFile("testdata/IM-0001-0003.dcm")

	// Create a context that will be cancelled
	ctx, cancel := context.WithCancel(context.Background())

	// Create a new provider for this test
	testParams := ServiceProviderParams{
		AETitle: "TEST_STORAGE_SCP",
		CStore:  testCStoreHandler,
	}

	testProvider, err := NewServiceProvider(testParams, ":0") // Use port 0 for automatic assignment
	require.NoError(t, err)

	// Start server in goroutine with context
	go testProvider.Run(ctx)

	// Give server time to start
	time.Sleep(100 * time.Millisecond)

	// Create client and connect to the test server
	su, err := NewServiceUser(ServiceUserParams{SOPClasses: sopclass.StorageClasses})
	require.NoError(t, err)
	defer su.Release()

	log.Printf("Connecting to test server at %v", testProvider.ListenAddr().String())
	su.Connect(testProvider.ListenAddr().String())

	// Perform C-STORE operation
	err = su.CStore(dataset)
	require.NoError(t, err)

	// Cancel context to stop server
	cancel()

	// Give server time to shutdown
	time.Sleep(100 * time.Millisecond)

	log.Printf("Context cancellation test completed successfully")
}

func TestServerClose(t *testing.T) {
	// Test server Close() method
	testParams := ServiceProviderParams{
		AETitle: "TEST_CLOSE_SCP",
		CStore:  testCStoreHandler,
	}

	testProvider, err := NewServiceProvider(testParams, ":0")
	require.NoError(t, err)

	// Start server with background context
	ctx := context.Background()
	go testProvider.Run(ctx)

	// Give server time to start
	time.Sleep(100 * time.Millisecond)

	// Close server
	err = testProvider.Close()
	require.NoError(t, err)

	log.Printf("Server Close() test completed successfully")
}

func TestContextCancellationInHandler(t *testing.T) {
	// Test that C-STORE handler respects context cancellation
	ctx, cancel := context.WithCancel(context.Background())

	// Create handler that will detect cancellation
	handlerCalled := false
	contextCancelled := false

	testHandler := func(
		ctx context.Context,
		connState ConnectionState,
		transferSyntaxUID string,
		sopClassUID string,
		sopInstanceUID string,
		dataReader io.Reader,
		dataSize int64) dimse.Status {

		handlerCalled = true

		// Check if context is cancelled
		select {
		case <-ctx.Done():
			contextCancelled = true
			return dimse.Status{
				Status:       dimse.CStoreCannotUnderstand,
				ErrorComment: "Server shutting down",
			}
		default:
		}

		return dimse.Success
	}

	testParams := ServiceProviderParams{
		AETitle: "TEST_CONTEXT_SCP",
		CStore:  testHandler,
	}

	testProvider, err := NewServiceProvider(testParams, ":0")
	require.NoError(t, err)

	// Start server with cancellable context
	go testProvider.Run(ctx)
	time.Sleep(50 * time.Millisecond)

	// Cancel context immediately
	cancel()
	time.Sleep(50 * time.Millisecond)

	// Try to connect to server (should fail or be rejected)
	su, err := NewServiceUser(ServiceUserParams{SOPClasses: sopclass.StorageClasses})
	require.NoError(t, err)
	defer su.Release()

	// This should fail because server is shutting down
	su.Connect(testProvider.ListenAddr().String())

	dataset := mustReadTestDICOMFile("testdata/IM-0001-0003.dcm")
	err = su.CStore(dataset)

	// We expect either an error or the handler to detect cancellation
	if err == nil && handlerCalled {
		require.True(t, contextCancelled, "Handler should have detected context cancellation")
	}

	log.Printf("Context cancellation in handler test completed")
}

func TestRunForeverBackwardCompatibility(t *testing.T) {
	// Test that RunForever works as expected for backward compatibility
	testParams := ServiceProviderParams{
		AETitle: "TEST_FOREVER_SCP",
		CStore:  testCStoreHandler,
	}

	testProvider, err := NewServiceProvider(testParams, ":0")
	require.NoError(t, err)

	// Start server with RunForever (should use background context internally)
	go testProvider.RunForever()

	// Give server time to start
	time.Sleep(100 * time.Millisecond)

	// Create client and test basic functionality
	su, err := NewServiceUser(ServiceUserParams{SOPClasses: sopclass.StorageClasses})
	require.NoError(t, err)
	defer su.Release()

	su.Connect(testProvider.ListenAddr().String())

	dataset := mustReadTestDICOMFile("testdata/IM-0001-0003.dcm")
	err = su.CStore(dataset)
	require.NoError(t, err)

	// Close server manually
	err = testProvider.Close()
	require.NoError(t, err)

	log.Printf("RunForever backward compatibility test completed successfully")
}

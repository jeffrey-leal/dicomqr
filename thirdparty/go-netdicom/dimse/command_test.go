package dimse

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestDimseCommand_AppendData(t *testing.T) {
	// Create temporary directory for test files
	tempDir, err := os.MkdirTemp("", "dimse_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	fpath := filepath.Join(tempDir, "test_command.dat")
	dc := NewDimseCommand(fpath)

	// Test appending data to non-existent file
	testData := []byte("test data 1")
	err = dc.AppendData(testData)
	if err != nil {
		t.Fatalf("AppendData failed: %v", err)
	}

	// Verify file was created and contains data
	content, err := os.ReadFile(fpath)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}
	if string(content) != "test data 1" {
		t.Errorf("Expected 'test data 1', got '%s'", string(content))
	}

	// Test appending more data
	additionalData := []byte("test data 2")
	err = dc.AppendData(additionalData)
	if err != nil {
		t.Fatalf("AppendData failed on second call: %v", err)
	}

	// Verify both data chunks are present
	content, err = os.ReadFile(fpath)
	if err != nil {
		t.Fatalf("Failed to read file after second append: %v", err)
	}
	expected := "test data 1test data 2"
	if string(content) != expected {
		t.Errorf("Expected '%s', got '%s'", expected, string(content))
	}
}

func TestDimseCommand_ReadData(t *testing.T) {
	// Create temporary directory for test files
	tempDir, err := os.MkdirTemp("", "dimse_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	fpath := filepath.Join(tempDir, "test_read.dat")
	dc := NewDimseCommand(fpath)

	// Write some test data directly to file
	testData := []byte("test read data")
	err = os.WriteFile(fpath, testData, 0644)
	if err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	// Test reading data
	reader := dc.ReadData()
	if reader == nil {
		t.Fatal("ReadData returned nil reader")
	}

	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("Failed to read from reader: %v", err)
	}

	if string(content) != "test read data" {
		t.Errorf("Expected 'test read data', got '%s'", string(content))
	}

	// Test reading again (should reset to beginning)
	reader2 := dc.ReadData()
	if reader2 == nil {
		t.Fatal("ReadData returned nil reader on second call")
	}

	content2, err := io.ReadAll(reader2)
	if err != nil {
		t.Fatalf("Failed to read from second reader: %v", err)
	}

	if string(content2) != "test read data" {
		t.Errorf("Expected 'test read data' on second read, got '%s'", string(content2))
	}
}

func TestDimseCommand_Ack(t *testing.T) {
	// Create temporary directory for test files
	tempDir, err := os.MkdirTemp("", "dimse_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	fpath := filepath.Join(tempDir, "test_ack.dat")
	dc := NewDimseCommand(fpath)

	// Write some test data
	testData := []byte("test ack data")
	err = os.WriteFile(fpath, testData, 0644)
	if err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(fpath); os.IsNotExist(err) {
		t.Fatal("Test file should exist before Ack")
	}

	// Call ReadData to open the file
	reader := dc.ReadData()
	if reader == nil {
		t.Fatal("ReadData returned nil reader")
	}

	// Test Ack - should close file and remove it
	err = dc.Ack()
	if err != nil {
		t.Fatalf("Ack failed: %v", err)
	}

	// Verify file was removed
	if _, err := os.Stat(fpath); !os.IsNotExist(err) {
		t.Error("File should have been removed by Ack")
	}
}

func TestDimseCommand_Close(t *testing.T) {
	// Create temporary directory for test files
	tempDir, err := os.MkdirTemp("", "dimse_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	fpath := filepath.Join(tempDir, "test_close.dat")
	dc := NewDimseCommand(fpath)

	// Write some test data
	testData := []byte("test close data")
	err = os.WriteFile(fpath, testData, 0644)
	if err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	// Call ReadData to open the file
	reader := dc.ReadData()
	if reader == nil {
		t.Fatal("ReadData returned nil reader")
	}

	// Test Close - should close file but not remove it
	err = dc.Close()
	if err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Verify file still exists (Close doesn't remove)
	if _, err := os.Stat(fpath); os.IsNotExist(err) {
		t.Error("File should still exist after Close")
	}
}

func TestDimseCommand_ConcurrentAccess(t *testing.T) {
	// Create temporary directory for test files
	tempDir, err := os.MkdirTemp("", "dimse_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	fpath := filepath.Join(tempDir, "test_concurrent.dat")
	dc := NewDimseCommand(fpath)

	// Test concurrent AppendData calls
	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func(index int) {
			data := []byte{byte(index)}
			err := dc.AppendData(data)
			if err != nil {
				t.Errorf("Concurrent AppendData failed: %v", err)
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines to complete
	for i := 0; i < 10; i++ {
		<-done
	}

	// Verify all data was written
	content, err := os.ReadFile(fpath)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}

	if len(content) != 10 {
		t.Errorf("Expected 10 bytes, got %d", len(content))
	}
}

func TestDimseCommand_AppendDataWithOpenReader(t *testing.T) {
	// Create temporary directory for test files
	tempDir, err := os.MkdirTemp("", "dimse_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	fpath := filepath.Join(tempDir, "test_append_with_reader.dat")
	dc := NewDimseCommand(fpath)

	// Write initial data
	initialData := []byte("initial data")
	err = os.WriteFile(fpath, initialData, 0644)
	if err != nil {
		t.Fatalf("Failed to write initial data: %v", err)
	}

	// Open reader (this should open the file)
	reader := dc.ReadData()
	if reader == nil {
		t.Fatal("ReadData returned nil reader")
	}

	// Verify dataReader is set
	if dc.dataReader == nil {
		t.Fatal("dataReader should be set after ReadData")
	}

	// Try to append data while reader is open
	appendData := []byte("appended data")
	err = dc.AppendData(appendData)
	if err != nil {
		t.Fatalf("AppendData failed: %v", err)
	}

	// Verify dataReader was closed
	if dc.dataReader != nil {
		t.Error("dataReader should be nil after AppendData")
	}

	// Verify all data is in the file
	content, err := os.ReadFile(fpath)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}

	expected := "initial dataappended data"
	if string(content) != expected {
		t.Errorf("Expected '%s', got '%s'", expected, string(content))
	}

	// Verify we can still read the data after append
	reader2 := dc.ReadData()
	if reader2 == nil {
		t.Fatal("ReadData should work after AppendData")
	}

	content2, err := io.ReadAll(reader2)
	if err != nil {
		t.Fatalf("Failed to read from reader after append: %v", err)
	}

	if string(content2) != expected {
		t.Errorf("Expected '%s' when reading after append, got '%s'", expected, string(content2))
	}
}

package dimse

import (
	"bytes"
	"io"
	"testing"

	"github.com/algm/go-netdicom/commandset"
	"github.com/algm/go-netdicom/pdu"
)

func createValidCStoreRqBytes() []byte {
	var buf bytes.Buffer
	cmd := &CStoreRq{
		AffectedSOPClassUID:    "1.2.840.10008.5.1.4.1.1.2",
		MessageID:              1,
		Priority:               0,
		CommandDataSetType:     CommandDataSetTypeNonNull,
		AffectedSOPInstanceUID: "1.2.3.4.5.6.7.8.9",
	}
	if err := EncodeMessage(&buf, cmd); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func createPDataTf(contextID byte, command bool, last bool, value []byte) *pdu.PDataTf {
	return &pdu.PDataTf{
		Items: []pdu.PresentationDataValueItem{{
			ContextID: contextID,
			Command:   command,
			Last:      last,
			Value:     value,
		}},
	}
}

func TestCommandAssembler(t *testing.T) {
	commandset.Init()

	t.Run("CommandWithData", func(t *testing.T) {
		commandBytes := createValidCStoreRqBytes()
		testData := []byte("test data payload")
		assembler := &CommandAssembler{}

		pdu1 := createPDataTf(1, true, true, commandBytes)
		contextID, msg, dc, err := assembler.AddDataPDU(pdu1)
		if err != nil {
			t.Fatalf("Command PDU failed: %v", err)
		}
		if contextID != 0 || msg != nil || dc != nil {
			t.Error("Expected incomplete assembly after command only")
		}

		pdu2 := createPDataTf(1, false, true, testData)
		contextID, msg, dc, err = assembler.AddDataPDU(pdu2)
		if err != nil {
			t.Fatalf("Data PDU failed: %v", err)
		}
		if contextID != 1 {
			t.Errorf("Expected contextID 1, got %d", contextID)
		}
		if msg == nil {
			t.Fatal("Expected message, got nil")
		}
		if dc == nil {
			t.Fatal("Expected DimseCommand, got nil")
		}
		b, _ := io.ReadAll(dc.ReadData())
		dc.Ack()
		if string(b) != string(testData) {
			t.Errorf("Expected data %s, got %s", string(testData), string(b))
		}
	})

	t.Run("MultiFragmentData", func(t *testing.T) {
		commandBytes := createValidCStoreRqBytes()
		testData := []byte("test data payload for multi fragment")
		mid := len(testData) / 2
		dataFragment1 := testData[:mid]
		dataFragment2 := testData[mid:]
		assembler := &CommandAssembler{}

		pdu1 := createPDataTf(1, true, true, commandBytes)
		contextID, msg, dc, err := assembler.AddDataPDU(pdu1)
		if err != nil {
			t.Fatalf("Command PDU failed: %v", err)
		}
		if contextID != 0 || msg != nil || dc != nil {
			t.Error("Expected incomplete assembly after command only")
		}

		pdu2 := createPDataTf(1, false, false, dataFragment1)
		contextID, msg, dc, err = assembler.AddDataPDU(pdu2)
		if err != nil {
			t.Fatalf("First data fragment failed: %v", err)
		}
		if contextID != 0 || msg != nil || dc != nil {
			t.Error("Expected incomplete assembly after first data fragment")
		}

		pdu3 := createPDataTf(1, false, true, dataFragment2)
		contextID, msg, dc, err = assembler.AddDataPDU(pdu3)
		if err != nil {
			t.Fatalf("Second data fragment failed: %v", err)
		}
		if contextID != 1 {
			t.Errorf("Expected contextID 1, got %d", contextID)
		}
		if msg == nil {
			t.Fatal("Expected message, got nil")
		}
		if dc == nil {
			t.Fatal("Expected DimseCommand, got nil")
		}
		b, _ := io.ReadAll(dc.ReadData())
		dc.Ack()
		if string(b) != string(testData) {
			t.Errorf("Expected data %s, got %s", string(testData), string(b))
		}
	})

	t.Run("MixedContextError", func(t *testing.T) {
		assembler := &CommandAssembler{}
		pdu1 := createPDataTf(1, true, false, []byte("test"))
		_, _, _, err := assembler.AddDataPDU(pdu1)
		if err != nil {
			t.Fatalf("First PDU failed: %v", err)
		}
		pdu2 := createPDataTf(2, true, true, []byte("test"))
		_, _, _, err = assembler.AddDataPDU(pdu2)
		if err == nil {
			t.Fatal("Expected error for mixed context, got nil")
		}
		if err.Error() != "mixed context: 1 2" {
			t.Errorf("Expected 'mixed context: 1 2', got '%s'", err.Error())
		}
	})

	t.Run("MultipleLastDataError", func(t *testing.T) {
		assembler := &CommandAssembler{}
		pdu1 := createPDataTf(1, false, true, []byte("test1"))
		_, _, _, err := assembler.AddDataPDU(pdu1)
		if err != nil {
			t.Fatalf("First data failed: %v", err)
		}
		pdu2 := createPDataTf(1, false, true, []byte("test2"))
		_, _, _, err = assembler.AddDataPDU(pdu2)
		if err == nil {
			t.Fatal("Expected error for multiple last data, got nil")
		}
		if err.Error() != "P_DATA_TF: found >1 data chunks with the Last bit set" {
			t.Errorf("Expected 'P_DATA_TF: found >1 data chunks with the Last bit set', got '%s'", err.Error())
		}
	})

	t.Run("CommandByteAccumulation", func(t *testing.T) {
		assembler := &CommandAssembler{}

		// Test command byte accumulation without parsing
		pdu1 := createPDataTf(1, true, false, []byte("part1"))
		_, _, _, err := assembler.AddDataPDU(pdu1)
		if err != nil {
			t.Fatalf("First fragment failed: %v", err)
		}

		// Check internal state
		if string(assembler.commandBytes) != "part1" {
			t.Errorf("Expected commandBytes 'part1', got '%s'", string(assembler.commandBytes))
		}
		if assembler.readAllCommand {
			t.Error("readAllCommand should be false after non-last fragment")
		}

		pdu2 := createPDataTf(1, true, false, []byte("part2"))
		_, _, _, err = assembler.AddDataPDU(pdu2)
		if err != nil {
			t.Fatalf("Second fragment failed: %v", err)
		}

		if string(assembler.commandBytes) != "part1part2" {
			t.Errorf("Expected commandBytes 'part1part2', got '%s'", string(assembler.commandBytes))
		}
	})
}

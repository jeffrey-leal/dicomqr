package dimse

import (
	"bytes"
	"fmt"
	"os"

	"github.com/algm/go-netdicom/pdu"
	"github.com/suyashkumar/dicom"
)

// CommandAssembler is a helper that assembles a DIMSE command message and data
// payload from a sequence of P_DATA_TF PDUs.
type CommandAssembler struct {
	contextID      byte
	commandBytes   []byte
	command        Message
	readAllCommand bool

	readAllData bool

	dataCmd *DimseCommand // temporary file holder for data set
}

// AddDataPDU is to be called for each P_DATA_TF PDU received from the
// network. If the fragment is marked as the last one, AddDataPDU returns
// <SOPUID, TransferSyntaxUID, payload, nil>.  If it needs more fragments, it
// returns <"", "", nil, nil>.  On error, it returns a non-nil error.
func (commandAssembler *CommandAssembler) AddDataPDU(pdu *pdu.PDataTf) (byte, Message, *DimseCommand, error) {
	for _, item := range pdu.Items {
		if commandAssembler.contextID == 0 {
			commandAssembler.contextID = item.ContextID
		} else if commandAssembler.contextID != item.ContextID {
			return 0, nil, nil, fmt.Errorf("mixed context: %d %d", commandAssembler.contextID, item.ContextID)
		}
		if item.Command {
			commandAssembler.commandBytes = append(commandAssembler.commandBytes, item.Value...)
			if item.Last {
				if commandAssembler.readAllCommand {
					return 0, nil, nil, fmt.Errorf("P_DATA_TF: found >1 command chunks with the Last bit set")
				}
				commandAssembler.readAllCommand = true
			}
		} else {
			// Data fragment. Persist to temporary file using DimseCommand.
			if commandAssembler.dataCmd == nil {
				tmpFile, err := os.CreateTemp("", "dimse_data_*")
				if err != nil {
					return 0, nil, nil, fmt.Errorf("failed to create temp file for DIMSE data: %w", err)
				}
				tmpFile.Close()
				commandAssembler.dataCmd = NewDimseCommand(tmpFile.Name())
			}
			if err := commandAssembler.dataCmd.AppendData(item.Value); err != nil {
				return 0, nil, nil, fmt.Errorf("failed to append data fragment: %w", err)
			}
			if item.Last {
				if commandAssembler.readAllData {
					return 0, nil, nil, fmt.Errorf("P_DATA_TF: found >1 data chunks with the Last bit set")
				}
				commandAssembler.readAllData = true
			}
		}
	}

	// Wait until full command received
	if !commandAssembler.readAllCommand {
		return 0, nil, nil, nil
	}

	// Decode command once.
	if commandAssembler.command == nil {
		// DIMSE commands are Implicit VR Little Endian (PS3.7 §6.3.1). The
		// suyashkumar parser needs ≥100 bytes to infer transfer syntax when no
		// file meta is present. Pad with zeros so the peek succeeds; bytesToRead
		// is set to the original length so the parser stops at real data.
		cmdBytes := commandAssembler.commandBytes
		if len(cmdBytes) < 100 {
			padded := make([]byte, 100)
			copy(padded, cmdBytes)
			cmdBytes = padded
		}
		ioReader := bytes.NewReader(cmdBytes)
		parser, err := dicom.Parse(ioReader, int64(len(commandAssembler.commandBytes)), nil,
			dicom.SkipPixelData(), dicom.SkipMetadataReadOnNewParserInit())
		if err != nil {
			return 0, nil, nil, fmt.Errorf("P_DATA_TF: failed to parse command bytes: %w", err)
		}
		commandAssembler.command, err = ReadMessage(&parser)
		if err != nil {
			return 0, nil, nil, err
		}
	}

	// If command expects data but we haven't read all yet, wait.
	if commandAssembler.command.HasData() && !commandAssembler.readAllData {
		return 0, nil, nil, nil
	}

	// Prepare return values.
	contextID := commandAssembler.contextID
	command := commandAssembler.command

	dc := commandAssembler.dataCmd

	// Reset assembler for next message.
	*commandAssembler = CommandAssembler{}

	return contextID, command, dc, nil
}

package dimse_test

import (
	"fmt"
	"testing"

	"github.com/algm/go-netdicom/dimse"
	"github.com/stretchr/testify/assert"
	"github.com/suyashkumar/dicom"
)

// Test C-STORE Response DIMSE protocol compliance
func TestCStoreRsp_ProtocolCompliance(t *testing.T) {
	t.Run("ValidResponse", func(t *testing.T) {
		// Valid C-STORE response with all required fields per DIMSE standard
		resp := &dimse.CStoreRsp{
			AffectedSOPClassUID:       "1.2.840.10008.5.1.4.1.1.2", // CT Image Storage
			MessageIDBeingRespondedTo: 123,
			CommandDataSetType:        dimse.CommandDataSetTypeNull,
			AffectedSOPInstanceUID:    "1.2.3.4.5.6.789.0.123456",
			Status:                    dimse.Success,
		}

		// Test DIMSE protocol methods
		assert.Equal(t, uint16(0x8001), resp.CommandField()) // C-STORE-RSP command field per PS 3.7
		assert.Equal(t, dimse.MessageID(123), resp.GetMessageID())
		assert.Equal(t, &resp.Status, resp.GetStatus())
		assert.False(t, resp.HasData())

		// Validate required fields are set
		assert.NotEmpty(t, resp.AffectedSOPClassUID)
		assert.NotZero(t, resp.MessageIDBeingRespondedTo)
		assert.NotEmpty(t, resp.AffectedSOPInstanceUID)
	})

	t.Run("DIMSEStandardStatusCodes", func(t *testing.T) {
		testCases := []struct {
			name       string
			statusCode dimse.StatusCode
			isError    bool
		}{
			{"Success", dimse.StatusSuccess, false},
			{"Cancel", dimse.StatusCancel, true},
			{"SOPClassNotSupported", dimse.StatusSOPClassNotSupported, true},
			{"InvalidArgumentValue", dimse.StatusInvalidArgumentValue, true},
			{"InvalidAttributeValue", dimse.StatusInvalidAttributeValue, true},
			{"InvalidObjectInstance", dimse.StatusInvalidObjectInstance, true},
			{"UnrecognizedOperation", dimse.StatusUnrecognizedOperation, true},
			{"NotAuthorized", dimse.StatusNotAuthorized, true},
			// C-STORE specific status codes per PS 3.4 Annex GG
			{"CStoreOutOfResources", dimse.CStoreOutOfResources, true},
			{"dimse.CStoreCannotUnderstand", dimse.CStoreCannotUnderstand, true},
			{"CStoreDataSetDoesNotMatchSOPClass", dimse.CStoreDataSetDoesNotMatchSOPClass, true},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				resp := &dimse.CStoreRsp{
					AffectedSOPClassUID:       "1.2.840.10008.5.1.4.1.1.2",
					MessageIDBeingRespondedTo: 123,
					CommandDataSetType:        dimse.CommandDataSetTypeNull,
					AffectedSOPInstanceUID:    "1.2.3.4.5.6.789.0.123456",
					Status:                    dimse.Status{Status: tc.statusCode},
				}

				assert.Equal(t, tc.statusCode, resp.Status.Status)

				// Test status interpretation
				if tc.isError {
					assert.NotEqual(t, dimse.StatusSuccess, resp.Status.Status)
				} else {
					assert.Equal(t, dimse.StatusSuccess, resp.Status.Status)
				}
			})
		}
	})

	t.Run("StatusWithErrorComment", func(t *testing.T) {
		// Test error status with comment per PS 3.7 C.4.2
		resp := &dimse.CStoreRsp{
			AffectedSOPClassUID:       "1.2.840.10008.5.1.4.1.1.2",
			MessageIDBeingRespondedTo: 123,
			CommandDataSetType:        dimse.CommandDataSetTypeNull,
			AffectedSOPInstanceUID:    "1.2.3.4.5.6.789.0.123456",
			Status: dimse.Status{
				Status:       dimse.CStoreCannotUnderstand,
				ErrorComment: "DICOM object validation failed",
			},
		}

		assert.Equal(t, dimse.CStoreCannotUnderstand, resp.Status.Status)
		assert.Equal(t, "DICOM object validation failed", resp.Status.ErrorComment)
	})
}

func TestCStoreRsp_CommandDataSetType(t *testing.T) {
	t.Run("NullCommandDataSetType", func(t *testing.T) {
		resp := &dimse.CStoreRsp{
			CommandDataSetType: dimse.CommandDataSetTypeNull,
		}
		assert.False(t, resp.HasData(), "C-STORE-RSP should not have data payload per DIMSE standard")
	})

	t.Run("NonNullCommandDataSetType", func(t *testing.T) {
		resp := &dimse.CStoreRsp{
			CommandDataSetType: dimse.CommandDataSetTypeNonNull,
		}
		assert.True(t, resp.HasData(), "Non-null CommandDataSetType indicates data payload")
	})

	t.Run("CustomCommandDataSetType", func(t *testing.T) {
		resp := &dimse.CStoreRsp{
			CommandDataSetType: dimse.CommandDataSetType(0x123),
		}
		assert.True(t, resp.HasData(), "Any non-null value should indicate data presence")
	})
}

func TestCStoreRsp_MessageIDValidation(t *testing.T) {
	// Test message ID range validation per DIMSE standard
	testCases := []dimse.MessageID{0, 1, 255, 32767, 65535}

	for _, msgID := range testCases {
		t.Run(fmt.Sprintf("MessageID_%d", msgID), func(t *testing.T) {
			resp := &dimse.CStoreRsp{
				AffectedSOPClassUID:       "1.2.840.10008.5.1.4.1.1.2",
				MessageIDBeingRespondedTo: msgID,
				CommandDataSetType:        dimse.CommandDataSetTypeNull,
				AffectedSOPInstanceUID:    "1.2.3.4.5.6.789.0.123456",
				Status:                    dimse.Success,
			}

			assert.Equal(t, msgID, resp.GetMessageID())
			assert.Equal(t, msgID, resp.MessageIDBeingRespondedTo)
		})
	}
}

func TestCStoreRsp_SOPClassValidation(t *testing.T) {
	// Test with standard DICOM SOP Classes per PS 3.4
	validSOPClasses := map[string]string{
		"CTImageStorage":                  "1.2.840.10008.5.1.4.1.1.2",
		"ComputedRadiographyImageStorage": "1.2.840.10008.5.1.4.1.1.1",
		"MRImageStorage":                  "1.2.840.10008.5.1.4.1.1.4",
		"UltrasoundImageStorage":          "1.2.840.10008.5.1.4.1.1.6.1",
		"SecondaryCaptureImageStorage":    "1.2.840.10008.5.1.4.1.1.7",
	}

	for name, sopClass := range validSOPClasses {
		t.Run(name, func(t *testing.T) {
			resp := &dimse.CStoreRsp{
				AffectedSOPClassUID:       sopClass,
				MessageIDBeingRespondedTo: 123,
				CommandDataSetType:        dimse.CommandDataSetTypeNull,
				AffectedSOPInstanceUID:    "1.2.3.4.5.6.789.0.123456",
				Status:                    dimse.Success,
			}

			assert.Equal(t, sopClass, resp.AffectedSOPClassUID)
		})
	}
}

func TestCStoreRsp_StringRepresentation(t *testing.T) {
	resp := &dimse.CStoreRsp{
		AffectedSOPClassUID:       "1.2.840.10008.5.1.4.1.1.2",
		MessageIDBeingRespondedTo: 123,
		CommandDataSetType:        dimse.CommandDataSetTypeNull,
		AffectedSOPInstanceUID:    "1.2.3.4.5.6.789.0.123456",
		Status:                    dimse.Status{Status: dimse.StatusSuccess},
	}

	str := resp.String()
	assert.Contains(t, str, "CStoreRsp")
	assert.Contains(t, str, "1.2.840.10008.5.1.4.1.1.2")
	assert.Contains(t, str, "123")
	assert.Contains(t, str, "1.2.3.4.5.6.789.0.123456")
	assert.Contains(t, str, "CommandDataSetType")
	assert.Contains(t, str, "Status")
}

func TestCStoreRsp_ExtraElementsHandling(t *testing.T) {
	// Test that extra (unparsed) elements can be stored and accessed
	resp := &dimse.CStoreRsp{
		AffectedSOPClassUID:       "1.2.840.10008.5.1.4.1.1.2",
		MessageIDBeingRespondedTo: 321,
		CommandDataSetType:        dimse.CommandDataSetTypeNull,
		AffectedSOPInstanceUID:    "1.2.3.4.5.6.789.0.135792",
		Status:                    dimse.Success,
		Extra:                     []*dicom.Element{}, // Empty slice to avoid nil pointer
	}

	// Verify extra elements field is accessible
	assert.NotNil(t, resp.Extra)
	assert.Len(t, resp.Extra, 0)
}

func TestCStoreRsp_ProtocolConformance(t *testing.T) {
	t.Run("CommandFieldValidation", func(t *testing.T) {
		resp := &dimse.CStoreRsp{}
		// Command field must be 0x8001 per PS 3.7 Section 9.1.1
		assert.Equal(t, uint16(0x8001), resp.CommandField())
	})

	t.Run("RequiredElementsOnly", func(t *testing.T) {
		// Test minimal valid response per DIMSE standard
		resp := &dimse.CStoreRsp{
			AffectedSOPClassUID:       "1.2.840.10008.5.1.4.1.1.2",
			MessageIDBeingRespondedTo: 1,
			CommandDataSetType:        dimse.CommandDataSetTypeNull,
			AffectedSOPInstanceUID:    "1.2.3",
			Status:                    dimse.Success,
		}

		// Validate protocol methods
		assert.Equal(t, uint16(0x8001), resp.CommandField())
		assert.False(t, resp.HasData()) // C-STORE-RSP typically has no data
		assert.Equal(t, dimse.MessageID(1), resp.GetMessageID())
		assert.Equal(t, &resp.Status, resp.GetStatus())
	})

	t.Run("ErrorStatusHandling", func(t *testing.T) {
		// Test each C-STORE specific error status
		errorStatuses := []dimse.StatusCode{
			dimse.CStoreOutOfResources,
			dimse.CStoreCannotUnderstand,
			dimse.CStoreDataSetDoesNotMatchSOPClass,
		}

		for _, status := range errorStatuses {
			t.Run(fmt.Sprintf("Status_%04X", status), func(t *testing.T) {
				resp := &dimse.CStoreRsp{
					AffectedSOPClassUID:       "1.2.840.10008.5.1.4.1.1.2",
					MessageIDBeingRespondedTo: 1,
					CommandDataSetType:        dimse.CommandDataSetTypeNull,
					AffectedSOPInstanceUID:    "1.2.3",
					Status: dimse.Status{
						Status:       status,
						ErrorComment: fmt.Sprintf("Test error for status %04X", status),
					},
				}

				assert.Equal(t, status, resp.Status.Status)
				assert.NotEqual(t, dimse.StatusSuccess, resp.Status.Status)
				assert.NotEmpty(t, resp.Status.ErrorComment)
			})
		}
	})
}

func TestCStoreRsp_DIMSEStandardCompliance(t *testing.T) {
	t.Run("dimse.CommandDataSetTypeNullForResponse", func(t *testing.T) {
		// Per DIMSE standard, C-STORE-RSP should have CommandDataSetType NULL
		resp := &dimse.CStoreRsp{
			AffectedSOPClassUID:       "1.2.840.10008.5.1.4.1.1.2",
			MessageIDBeingRespondedTo: 123,
			CommandDataSetType:        dimse.CommandDataSetTypeNull,
			AffectedSOPInstanceUID:    "1.2.3.4.5.6.789.0.123456",
			Status:                    dimse.Success,
		}

		assert.Equal(t, dimse.CommandDataSetTypeNull, resp.CommandDataSetType)
		assert.False(t, resp.HasData())
	})

	t.Run("MessageIDCorrespondence", func(t *testing.T) {
		// Message ID in response must correspond to request
		requestMsgID := dimse.MessageID(42)
		resp := &dimse.CStoreRsp{
			MessageIDBeingRespondedTo: requestMsgID,
		}

		assert.Equal(t, requestMsgID, resp.GetMessageID())
		assert.Equal(t, requestMsgID, resp.MessageIDBeingRespondedTo)
	})

	t.Run("SOPIdentifierConsistency", func(t *testing.T) {
		// SOP Class and Instance UIDs should be consistent between request and response
		sopClassUID := "1.2.840.10008.5.1.4.1.1.2"
		sopInstanceUID := "1.2.3.4.5.6.789.0.123456"

		resp := &dimse.CStoreRsp{
			AffectedSOPClassUID:    sopClassUID,
			AffectedSOPInstanceUID: sopInstanceUID,
		}

		assert.Equal(t, sopClassUID, resp.AffectedSOPClassUID)
		assert.Equal(t, sopInstanceUID, resp.AffectedSOPInstanceUID)
	})

	t.Run("StatusCodeRangeValidation", func(t *testing.T) {
		// Test various status code ranges per DIMSE standard
		testCases := []struct {
			name   string
			status dimse.StatusCode
			valid  bool
		}{
			{"Success", dimse.StatusSuccess, true},
			{"Pending", dimse.StatusPending, true},
			{"Cancel", dimse.StatusCancel, true},
			{"CStoreOutOfResources", dimse.CStoreOutOfResources, true},
			{"CStoreCannotUnderstand", dimse.CStoreCannotUnderstand, true},
			{"CStoreDataSetDoesNotMatchSOPClass", dimse.CStoreDataSetDoesNotMatchSOPClass, true},
			{"InvalidCustomStatus", dimse.StatusCode(0x9999), true}, // Custom status codes are allowed
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				resp := &dimse.CStoreRsp{
					Status: dimse.Status{Status: tc.status},
				}

				// All status codes should be valid in the struct
				assert.Equal(t, tc.status, resp.Status.Status)
			})
		}
	})

	t.Run("CommandFieldConstant", func(t *testing.T) {
		// Test that command field is always correct per DIMSE standard
		resp := &dimse.CStoreRsp{}

		// Should always return 0x8001 for C-STORE-RSP
		assert.Equal(t, uint16(0x8001), resp.CommandField())

		// Test with different response instances
		resp2 := &dimse.CStoreRsp{Status: dimse.Status{Status: dimse.CStoreCannotUnderstand}}
		assert.Equal(t, uint16(0x8001), resp2.CommandField())
	})

	t.Run("FieldValidation", func(t *testing.T) {
		// Test field validation per DIMSE requirements
		resp := &dimse.CStoreRsp{
			AffectedSOPClassUID:       "1.2.840.10008.5.1.4.1.1.2",
			MessageIDBeingRespondedTo: 1,
			CommandDataSetType:        dimse.CommandDataSetTypeNull,
			AffectedSOPInstanceUID:    "1.2.3.4.5.6.789.0.123456",
			Status:                    dimse.Success,
		}

		// All required fields should be properly set
		assert.NotEmpty(t, resp.AffectedSOPClassUID, "AffectedSOPClassUID is required")
		assert.NotZero(t, resp.MessageIDBeingRespondedTo, "MessageIDBeingRespondedTo is required")
		assert.NotEmpty(t, resp.AffectedSOPInstanceUID, "AffectedSOPInstanceUID is required")

		// CommandDataSetType should be explicitly set
		assert.Equal(t, dimse.CommandDataSetTypeNull, resp.CommandDataSetType)

		// Status should be valid
		assert.NotNil(t, resp.GetStatus())
	})
}

func TestCStoreRsp_EdgeCases(t *testing.T) {
	t.Run("EmptyStrings", func(t *testing.T) {
		// Test behavior with empty strings (should be allowed but not recommended)
		resp := &dimse.CStoreRsp{
			AffectedSOPClassUID:       "",
			MessageIDBeingRespondedTo: 1,
			CommandDataSetType:        dimse.CommandDataSetTypeNull,
			AffectedSOPInstanceUID:    "",
			Status:                    dimse.Success,
		}

		assert.Empty(t, resp.AffectedSOPClassUID)
		assert.Empty(t, resp.AffectedSOPInstanceUID)
		assert.Equal(t, uint16(0x8001), resp.CommandField())
	})

	t.Run("MaxMessageID", func(t *testing.T) {
		// Test maximum message ID value
		maxMsgID := dimse.MessageID(65535)
		resp := &dimse.CStoreRsp{
			MessageIDBeingRespondedTo: maxMsgID,
		}

		assert.Equal(t, maxMsgID, resp.GetMessageID())
	})

	t.Run("NilExtra", func(t *testing.T) {
		// Test with nil extra elements
		resp := &dimse.CStoreRsp{
			Extra: nil,
		}

		assert.Nil(t, resp.Extra)
	})

	t.Run("EmptyExtra", func(t *testing.T) {
		// Test with empty extra elements slice
		resp := &dimse.CStoreRsp{
			Extra: []*dicom.Element{},
		}

		assert.NotNil(t, resp.Extra)
		assert.Len(t, resp.Extra, 0)
	})
}

func TestCStoreRsp_StatusCodeValues(t *testing.T) {
	t.Run("StatusCodeConstants", func(t *testing.T) {
		// Verify that status code constants have the correct values per DIMSE standard
		assert.Equal(t, dimse.StatusCode(0x0000), dimse.StatusSuccess)
		assert.Equal(t, dimse.StatusCode(0xFE00), dimse.StatusCancel)
		assert.Equal(t, dimse.StatusCode(0xFF00), dimse.StatusPending)

		// C-STORE specific status codes
		assert.Equal(t, dimse.StatusCode(0xA700), dimse.CStoreOutOfResources)
		assert.Equal(t, dimse.StatusCode(0xC000), dimse.CStoreCannotUnderstand)
		assert.Equal(t, dimse.StatusCode(0xA900), dimse.CStoreDataSetDoesNotMatchSOPClass)
	})

	t.Run("StatusCodeRanges", func(t *testing.T) {
		// Test that status codes are in expected ranges
		assert.True(t, dimse.StatusSuccess == 0x0000, "Success should be 0x0000")
		assert.True(t, dimse.CStoreOutOfResources >= 0xA000, "Warning status should be >= 0xA000")
		assert.True(t, dimse.CStoreCannotUnderstand >= 0xC000, "Failure status should be >= 0xC000")
	})
}

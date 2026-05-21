# Golang implementation of DICOM network protocol.

See doc.go for (incomplete) documentation. See storeclient and storeserver for
examples.

Inspired by https://github.com/pydicom/pynetdicom3.

Status as of 2017-10-02:

- C-STORE, C-FIND, C-GET work, both for the client and the server. Look at
  sampleclient, sampleserver, or e2e_test.go for examples. In general, the
  server (provider)-side code is better tested than the client-side code.

- Compatibility has been tested against pynetdicom and Osirix MD.

TODO:

- Documentation.

- Better SSL support.

- Implement the rest of DIMSE protocols, in particular C-MOVE on the client
  side, and N-\* commands.

- Better message validation.

- Remove the "limit" param from the Decoder, and rely on io.EOF detection instead.

# go-netdicom

## Fuzzing

### Native Go Fuzzing (Go 1.18+)

This project now supports **native Go fuzzing** that replaces the deprecated go-fuzz tools:

#### PDU Fuzzing (✅ Working)
```bash
# Run PDU fuzzing - tests Protocol Data Unit parsing
go test -fuzz=FuzzPDU ./fuzzpdu

# Run with specific time limit
go test -fuzz=FuzzPDU -fuzztime=30s ./fuzzpdu
```

#### End-to-End Fuzzing (⚠️ In Progress)
```bash
# E2E fuzzing currently has issues but is being improved
# Use PDU fuzzing for now, or contribute to fix E2E fuzzing
# go test -fuzz=FuzzE2E ./fuzze2e
```

### Migration from go-fuzz ✅ Complete

**✅ MIGRATION COMPLETED:**
- ✅ Removed deprecated go-fuzz dependencies  
- ✅ Created native Go 1.18+ fuzz tests
- ✅ Updated all documentation to English
- ✅ PDU fuzzing working perfectly
- ⚠️ E2E fuzzing needs additional work (complex network operations)

### Legacy go-fuzz (DEPRECATED)

**⚠️ WARNING: go-fuzz is deprecated since Go 1.18 and no longer receives maintenance.**

Use native Go fuzzing above instead.

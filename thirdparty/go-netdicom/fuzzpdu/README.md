# PDU Fuzzing

This directory contains fuzz tests for PDU (Protocol Data Unit) parsing using **native Go fuzzing** (Go 1.18+).

## Running the Fuzzer

```bash
# Run the PDU fuzzer
go test -fuzz=FuzzPDU .

# Run with specific time limit
go test -fuzz=FuzzPDU -fuzztime=30s .

# Run with maximum CPU cores
go test -fuzz=FuzzPDU -fuzztime=1m .
```

## About

The fuzzer tests PDU parsing by:
1. Feeding random data to the PDU parser
2. Testing both PDU format and DICOM dataset parsing paths
3. Discovering crashes and hangs in the parsing logic

## Legacy go-fuzz (DEPRECATED)

**⚠️ WARNING: go-fuzz is deprecated since Go 1.18 and no longer maintained.**

The old go-fuzz approach is no longer recommended. Use the native fuzzing above instead.

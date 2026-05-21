# End-to-End Fuzzing

This directory contains fuzz tests using **native Go fuzzing** (Go 1.18+).

## Status

⚠️ **Current Status**: The E2E fuzzer is currently having issues with native Go fuzzing due to the complexity of the network operations and real DICOM file requirements. The fuzzer discovers real bugs (like dictionary lookup failures) but crashes during baseline coverage gathering.

**Working alternatives:**
- Use `fuzzpdu/` for PDU-level fuzzing (✅ Works)
- Manual testing with the legacy `Fuzz(data)` function

## Running the Fuzzer (When Fixed)

```bash
# Run the end-to-end fuzzer (currently not working)
go test -fuzz=FuzzE2E .

# Run with specific time limit
go test -fuzz=FuzzE2E -fuzztime=30s .

# Run with maximum CPU cores
go test -fuzz=FuzzE2E -fuzztime=1m .
```

## About

The fuzzer tests the complete DICOM network communication stack by:
1. Starting a DICOM server with fault injection
2. Running a DICOM client against it
3. Using fuzzed data to trigger various code paths and potential bugs

## Legacy go-fuzz (DEPRECATED)

**⚠️ WARNING: go-fuzz is deprecated since Go 1.18 and no longer maintained.**

The old approach used:
```bash
# DEPRECATED - do not use
go-fuzz-build github.com/yasushi-saito/go-netdicom/fuzze2e
go-fuzz -bin fuzze2e-fuzz.zip -workdir /tmp/fuzze2e
```

## TODO

- [ ] Fix E2E fuzzer to work with native Go fuzzing
- [ ] Handle network setup/teardown properly in fuzz tests
- [ ] Add more targeted unit-level fuzz tests

# Login lifecycle process probes

macOS probes using synthetic credentials, isolated HOME and loopback HTTP only. No real browser or provider requests. Requires Python 3 and macOS lsof. Temporary files use TMPDIR; set it to a private scratch directory.

Build the gateway, then run each script with the absolute binary path and an output JSON path outside the repository:

```sh
python3 tools/qa009/cli_probe.py /absolute/path/trae-api /scratch/cli.json
python3 tools/qa009/process_probe.py /absolute/path/trae-api /scratch/process.json
python3 tools/qa009/fake_identity_probe.py /absolute/path/trae-api /scratch/identity.json
```

Probes cover command surface, foreground Auth lifecycle, existing Auth reuse, API read-only state, occupied ports, process cleanup and rejected forged instance identity. Renewal and replacement-listener regressions also live in qa_trae009_contract_test.go. These checks do not close the pre-existing refresh defects in docs/KNOWN_ISSUES.md or verify official login.

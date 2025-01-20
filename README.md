# avm-version-check

This tool is specifically designed for the **AVM (Azure Verified Modules)** team for Terraform. It automates the process of:

- Monitoring multiple Azure Verified Module repositories.
- Checking Terraform provider version compatibility (especially for `azurerm` and `azapi`).
- Gathering metadata (e.g., last commit date and author).
- Generating a JSON report for further analysis.

While this tool is _not_ intended to be a generic solution for all Terraform modules, it could inspire others who need to manage multiple repositories and track Terraform provider versions.

## Features

1. **Download/Update the Source CSV**  
   Fetches a CSV listing Terraform modules/repositories from a remote URL.  

2. **Process CSV Entries**  
   - Clones each repo and inspects the Terraform module constraints.
   - Checks for compatibility against known minimum provider versions.
   - Collects metadata (last commit date and author).
   - Outputs a JSON file with all results.

3. **Analysis**  
   - Provides a built-in subcommand for additional statistics, such as:
     - **Unreachable** repositories (those that fail to clone).
     - **Dormant** repositories (no commits in the last 6 months).
     - **Not-compatible** repositories (e.g., `azurerm` or `azapi` constraints not satisfied).
   - Prints colorized summaries to the console.

## Installation

1. **Install Go** (1.23+ recommended).
2. Clone this repository:

   ```bash
   git clone https://github.com/Nepomuceno/avm-version-check.git
   cd avm-version-check
   ```

3. **Build**:

   ```bash
   go build -o avm-version-check main.go
   ```

4. **Check** the CLI help:

   ```bash
   ./avm-version-check --help
   ```

## Usage

### 1. Update the Source CSV

```bash
./avm-version-check update-source \
    --url https://raw.githubusercontent.com/Azure/Azure-Verified-Modules/refs/heads/main/docs/static/module-indexes/TerraformResourceModules.csv \
    --output modules.csv
```

- Use `--force` to overwrite `modules.csv` if it already exists.

### 2. Process the CSV

```bash
./avm-version-check process \
    --input modules.csv \
    --output output.json \
    --workers 5 \
    --download \
    --quiet
```

- **`--download`** fetches the CSV file **before** processing, even if you already have it locally.  
- **`--quiet`** suppresses log warnings so you primarily see the progress bar and a final summary.
- **`--workers`** determines concurrency when cloning/parsing repositories.

At the end of processing, the tool prints a summary of unreachable, dormant, and not-compatible repositories directly to the console.

### 3. Analysis

If you want a **more detailed** analysis with colorized output, run:

```bash
./avm-version-check analysis --input output.json
```

This will print detailed lists of:
- **Unreachable** repositories
- **Dormant** repositories
- **Not-compatible** repositories

## Contributing

We welcome improvements and suggestions! Feel free to open issues and PRs that may help the AVM team or even expand this tool’s applicability.

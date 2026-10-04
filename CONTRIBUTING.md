# Contributing to OpenClaw Kubernetes Operator

First off, thank you for considering contributing to the OpenClaw Kubernetes Operator! It's people like you that make this project great.

## Code of Conduct

This project and everyone participating in it is governed by our [Code of Conduct](CODE_OF_CONDUCT.md). By participating, you are expected to uphold this code.

## How Can I Contribute?

### Reporting Bugs

Before creating bug reports, please check the existing issues to avoid duplicates. When you create a bug report, include as many details as possible using our [bug report template](.github/ISSUE_TEMPLATE/bug_report.md).

**Great bug reports include:**
- A quick summary and/or background
- Steps to reproduce (be specific!)
- What you expected would happen
- What actually happens
- Kubernetes version, operator version, and other relevant environment details
- Notes (possibly including why you think this might be happening)

### Suggesting Enhancements

Enhancement suggestions are tracked as GitHub issues. When creating an enhancement suggestion, use our [feature request template](.github/ISSUE_TEMPLATE/feature_request.md) and include:

- A clear and descriptive title
- A detailed description of the proposed enhancement
- Explain why this enhancement would be useful
- List any alternatives you've considered

### Pull Requests

1. Fork the repo and create your branch from `main`
2. If you've added code that should be tested, add tests
3. If you've changed APIs, update the documentation
4. Ensure the test suite passes
5. Make sure your code lints
6. Issue that pull request!

## Development Setup

### Prerequisites

- Go 1.22+
- Node.js 22+ (verified plugin installer tests)
- Docker
- kubectl
- Kind (for local testing)
- Make

### Getting Started

```bash
# Clone your fork
git clone https://github.com/YOUR_USERNAME/openclaw-operator.git
cd openclaw-operator

# Add upstream remote
git remote add upstream https://github.com/paperclipinc/openclaw-operator.git

# Install dependencies
go mod download

# Generate code and manifests
make generate manifests

# Run tests
make test

# Run linter
make lint
```

### Running Locally

```bash
# Create a Kind cluster
kind create cluster

# Install CRDs
make install

# Run the operator locally (outside the cluster)
make run
```

### Testing Changes

```bash
# Run unit tests
make test

# Run linter
make lint

# Run E2E tests (requires Kind)
make test-e2e
```

### Building

```bash
# Build the binary
make build

# Build Docker image
make docker-build IMG=my-operator:dev

# Load into Kind
kind load docker-image my-operator:dev

# Deploy to Kind
make deploy IMG=my-operator:dev
```

## Style Guidelines

### Git Commit Messages

- Use the present tense ("Add feature" not "Added feature")
- Use the imperative mood ("Move cursor to..." not "Moves cursor to...")
- Limit the first line to 72 characters or less
- Reference issues and pull requests liberally after the first line
- Consider using [Conventional Commits](https://www.conventionalcommits.org/):
  - `feat:` for new features
  - `fix:` for bug fixes
  - `docs:` for documentation changes
  - `chore:` for maintenance tasks
  - `test:` for test additions/changes
  - `refactor:` for code refactoring

### Go Code Style

- Follow the [Effective Go](https://golang.org/doc/effective_go) guidelines
- Run `make fmt` before committing
- Run `make lint` and fix any issues
- Write meaningful comments for exported functions
- Keep functions focused and small

### Kubernetes Resources

- Follow [Kubernetes API conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md)
- Use meaningful names for CRD fields
- Provide sensible defaults where appropriate
- Document all CRD fields with `// +kubebuilder:validation` markers

## Project Structure

```
.
├── api/v1alpha1/          # CRD type definitions
├── cmd/                   # Main entrypoint
├── config/                # Kubernetes manifests
│   ├── crd/              # CRD definitions
│   ├── manager/          # Operator deployment
│   ├── rbac/             # RBAC configuration
│   └── samples/          # Example CRs
├── internal/
│   ├── controller/       # Reconciliation logic
│   ├── resources/        # Resource builders
│   └── webhook/          # Admission webhooks
├── charts/               # Helm chart
└── test/e2e/            # E2E tests
```

## Review Process

1. All submissions require review from a maintainer
2. We use GitHub pull request reviews
3. CI must pass before merging
4. At least one approval is required

## Community

- GitHub Issues: For bugs and feature requests
- GitHub Discussions: For questions and general discussion
- Pull Requests: For code contributions

## Recognition

Contributors will be recognized in our releases and in the project's contributors list.

Thank you for contributing!

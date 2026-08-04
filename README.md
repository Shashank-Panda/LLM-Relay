# Relay

> **An AI Gateway & Control Plane for Intelligent Multi-LLM Routing**

---

# Vision

Relay is an AI Gateway that abstracts multiple LLM providers behind a single, provider-agnostic API.

Rather than simply forwarding requests to different providers, Relay acts as an intelligent decision engine that determines the optimal model based on factors such as:

* Prompt characteristics
* Provider capabilities
* Cost
* Latency
* Context window
* Reliability
* Organization policies
* Historical performance

The long-term objective is to build infrastructure that could realistically be deployed inside an organization as the central platform for AI consumption.

---

# Project Goals

## Functional Goals

* Unified API for multiple LLM providers
* Intelligent prompt classification
* Dynamic provider routing
* Explainable routing decisions
* Provider failover
* Streaming support
* Cost optimization
* Observability
* Enterprise-ready architecture

## Non-Functional Goals

* Extensible
* Highly testable
* Idiomatic Go
* Provider agnostic
* Easily configurable
* Production-oriented
* OpenAI-compatible API
* Clean architecture
* Low coupling
* High cohesion

---

# High-Level Architecture

```
                  Clients

    CLI
    VS Code
    Slack
    REST API
    Future SDKs

                │
                ▼

        API Gateway Layer

                │

                ▼

      Request Validation

                │

                ▼

      Prompt Classification

                │

                ▼

       Prompt Analysis

                │

                ▼

        Routing Engine

                │

                ▼

     Policy Enforcement

                │

                ▼

    Provider Selection

                │

                ▼

 Provider Abstraction Layer

      ┌──────────────┐
      │ OpenAI       │
      │ Anthropic    │
      │ Gemini       │
      │ Ollama       │
      │ Future       │
      └──────────────┘

                │

                ▼

      Streaming Response

                │

                ▼

     Response Normalization

                │

                ▼

              Client
```

---

# Core Components

## 1. API Gateway

Responsibilities

* Accept requests
* Validate requests
* Authentication
* Streaming
* Response normalization
* Error handling

---

## 2. Provider Abstraction Layer

Every provider should implement a common interface.

Example:

* Chat
* Streaming Chat
* Embeddings
* Images (future)
* Audio (future)

Goals

* Plug-and-play providers
* Easy provider onboarding
* No provider-specific logic outside adapters

---

## 3. Prompt Classification Engine

Classify every prompt before routing.

Possible outputs:

* Task Type
* Programming Language
* Framework
* Domain
* Reasoning Complexity
* Vision Requirement
* Context Window Requirement
* Estimated Tokens
* Confidence Score
* Streaming Recommendation

Example task categories:

* Code Generation
* Code Review
* Debugging
* Refactoring
* Architecture
* Summarization
* Translation
* Research
* Creative Writing
* SQL
* Data Analysis
* Legal
* Medical
* OCR

The classifier should evolve over time:

V1

* Rule-based

V2

* Rule + heuristics

V3

* Lightweight AI classifier

V4

* Self-improving classifier

---

# Prompt Analysis

Estimate

* Cost
* Latency
* Token usage
* Temperature recommendation
* Max token recommendation
* Context length
* Complexity

---

# Routing Engine

The routing engine is the core of Relay.

It should never contain provider-specific logic.

Instead it receives structured metadata from the classifier.

Example inputs:

* Task type
* Complexity
* Estimated tokens
* Required capabilities
* User preferences
* Organization policies

The routing engine should score every provider.

Possible scoring factors:

Provider Quality

* Coding quality
* Reasoning quality
* Vision quality
* Long context capability
* Tool calling support

Performance

* Live latency
* Historical latency
* Success rate
* Failure rate

Cost

* Input token cost
* Output token cost
* Estimated request cost

Context

* Maximum context window
* Streaming support

Policies

* Organization restrictions
* Provider allow/block lists

Preferences

* User preference
* Team preference

The routing engine should return:

* Selected provider
* Final score
* Provider rankings
* Routing explanation

---

# Explainable Routing

Every routing decision should be transparent.

Example:

Selected Provider

Reason:

* Coding Quality
* Cost
* Latency
* Context
* Policy

Provider Rankings

* Claude
* GPT
* Gemini

Scoring breakdown

This should be accessible via an API for debugging.

---

# Reliability

Features

* Retry mechanism
* Exponential backoff
* Circuit breaker
* Health checks
* Automatic provider failover
* Request timeouts
* Provider isolation
* Recovery after health restoration

---

# Streaming

Support token streaming.

Requirements

* Provider-independent streaming
* Response normalization
* Graceful cancellation
* Backpressure handling
* Streaming metrics

---

# Provider Management

Support

* Dynamic provider registration
* Provider enable/disable
* Model mapping
* Default provider
* Provider priority
* Provider capability registry

---

# Caching

Possible cache layers

* Prompt hash cache
* Response cache
* Redis cache
* Semantic cache (future)

Metrics

* Cache hits
* Cache misses
* Hit ratio

---

# Observability

Metrics

* Request count
* Token usage
* Cost
* Latency
* TTFT
* Streaming duration
* Provider usage
* Provider failures
* Retry count
* Cache metrics

Logging

* Request logs
* Provider logs
* Routing logs
* Error logs

Tracing

* Request lifecycle
* Provider calls
* Routing decisions

Integrations

* Prometheus
* Grafana
* OpenTelemetry

---

# Learning & Analytics

Store historical routing information.

Metrics

* Provider success rate
* Provider latency
* Provider cost
* Provider usage
* User feedback

Future goal

Allow routing to improve using historical data instead of static rules.

---

# Multi-Provider Strategies

Future support

* Parallel execution
* Fastest response wins
* Consensus routing
* Majority voting
* Best-answer selection
* Response merging

---

# Enterprise Features

Authentication

* API Keys
* JWT
* OAuth

Authorization

* RBAC

Organization Support

* Teams
* Organizations
* User quotas

Budgets

* Daily budget
* Monthly budget

Policies

* Provider restrictions
* PII rules
* Cost policies

Audit Logs

Track

* User
* Time
* Provider
* Tokens
* Cost

---

# Security

* Prompt sanitization
* Secret detection
* Secret masking
* PII detection
* Request validation
* Input limits
* Output filtering

---

# Configuration

Configuration should support

* YAML
* Environment variables
* Runtime reload
* Routing policies
* Provider configuration
* Feature flags

---

# Developer Experience

* Docker
* Docker Compose
* GitHub Actions
* Swagger/OpenAPI
* Postman Collection
* Unit Tests
* Integration Tests
* Mock Providers

---

# Deployment

Initial

* Local Docker

Later

* Cloud Run
* Fly.io
* Render

Eventually

* Kubernetes

---

# Technology Stack

Language

* Go

HTTP

* Chi

Configuration

* Viper

Logging

* Zap

Caching

* Redis

Metrics

* Prometheus

Tracing

* OpenTelemetry

Documentation

* Swagger/OpenAPI

Testing

* Go Testing
* Table-driven tests

Containerization

* Docker

CI/CD

* GitHub Actions

---

# Design Principles

* Clean Architecture
* SOLID Principles
* Dependency Inversion
* Interface-driven design
* Composition over inheritance
* Provider independence
* Configuration over hardcoding
* High cohesion
* Low coupling

---

# Explicit Non-Goals (Initial Versions)

The following are intentionally out of scope for early iterations:

* RAG
* Vector databases
* Agent frameworks
* Workflow automation
* Fine-tuning models
* Custom LLM training
* Frontend dashboard (until backend is mature)

---

# Questions for Architecture Review

Please review this architecture critically as if you were a Principal Engineer or Staff Engineer responsible for approving it for production.

Specifically evaluate:

1. Overall architecture and separation of concerns.
2. Extensibility for adding new providers and routing strategies.
3. Scalability under high request volume.
4. Concurrency model and potential bottlenecks.
5. Streaming architecture.
6. Reliability and fault tolerance.
7. Testability of each component.
8. Configuration management.
9. Any unnecessary complexity or over-engineering.
10. Missing components required for a production-grade AI Gateway.
11. Alternative architectural approaches and their trade-offs.
12. Recommended implementation order to maximize learning while minimizing rework.

Provide brutally honest feedback and suggest improvements before any implementation begins.

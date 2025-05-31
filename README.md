# Gorizond Kubernetes Operator

## Overview

The **Gorizond Kubernetes Operator** automates provisioning and managing of distributed K3s clusters integrated with Headscale for secure networking via Tailscale. By leveraging Rancher Fleet GitOps, the operator provides a unified, secure Kubernetes API accessible globally, simplifying distributed deployments and edge computing.

![gorizond-cluster-controller](https://github.com/user-attachments/assets/66ff313e-8ef3-49ef-b32d-852b32593333)


## Key Features

* **Custom Resource Definitions (CRD)**: Define clusters declaratively using the `Cluster` CRD.
* **Headscale Integration**: Securely connect nodes via Tailscale mesh VPN managed by Headscale.
* **Fleet GitOps**: Declaratively manage clusters and deployments through Rancher Fleet.
* **Dynamic Node Provisioning**: Easily add remote K3s worker nodes using install scripts.

## Architecture

* The operator watches Gorizond CRDs (`clusters.provisioning.gorizond.io`) and provisions new K3s clusters accordingly.
* Each cluster receives a dedicated Headscale server instance for secure node communication.
* An nginx-based `install-server` Helm chart generates installation scripts to facilitate easy setup of Tailscale and K3s nodes.
* Nodes securely connect using Tailscale, enabling seamless addition of workers anywhere with internet access.

## Getting Started

### Prerequisites

* Kubernetes cluster managed by Rancher
* Rancher Fleet enabled
* PostgreSQL/MySQL databases

### Installation

Install Gorizond Operator via Helm:

```bash
helm repo add gorizond https://gorizond.github.io/fleet-gorizond-charts/
helm repo update

helm install gorizond-controller gorizond/gorizond-cluster-controller -n cattle-system --create-namespace \
  --set env[0].name=DB_DSN_HEADSCALE,env[0].value="postgres://user:pass@host:port/db?sslmode=disable" \
  --set env[1].name=DB_DSN_KUBERNETES,env[1].value="mysql://user:pass@host:port/db" \
  --set env[2].name=GORIZOND_DOMAIN_HEADSCALE,env[2].value="headscale.example.com" \
  --set env[3].name=GORIZOND_DOMAIN_K3S,env[3].value="k3s.example.com"
```

### Provision a Cluster

Create a cluster resource:

```yaml
apiVersion: provisioning.gorizond.io/v1
kind: Cluster
metadata:
  name: my-cluster
spec:
  kubernetesVersion: "v1.27.1+k3s1"
```

Apply this manifest:

```bash
kubectl apply -f cluster.yaml
```

Monitor the status:

```bash
kubectl get gorizond my-cluster -o yaml
```

## Adding Worker Nodes

The operator's Helm chart (`install-server`) generates a simple nginx server that provides an installation script. This script:

* Installs the Tailscale binary.
* Runs the K3s installation command:

This simplifies adding worker nodes globally; nodes only require internet access.

## Benefits

* **Unified Kubernetes API** across distributed clusters
* **Simplified node onboarding**
* **Secure networking** through Headscale/Tailscale
* **Centralized management** via Rancher Fleet

Enjoy seamless cluster provisioning with Gorizond!

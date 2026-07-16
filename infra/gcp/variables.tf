variable "project_id" {
  description = "Existing, billing-enabled Google Cloud project."
  type        = string
  default     = "agentops-open-course"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "project_id must be a valid Google Cloud project ID."
  }
}

variable "region" {
  description = "Region for the VPC subnet, registry, and artifact bucket."
  type        = string
  default     = "europe-west1"
}

variable "zone" {
  description = "Single GKE zone; a zonal cluster avoids multi-zone node cost."
  type        = string
  default     = "europe-west1-b"
}

variable "cluster_name" {
  description = "GKE cluster name."
  type        = string
  default     = "agentops"
}

variable "machine_type" {
  description = "Spot node machine type. e2-standard-2 is the tested course size."
  type        = string
  default     = "e2-standard-2"
}

variable "spot_nodes" {
  description = "Use interruptible Spot capacity for the single course node."
  type        = bool
  default     = true
}

variable "node_disk_size_gb" {
  description = "Boot disk size for the single node."
  type        = number
  default     = 30

  validation {
    condition     = var.node_disk_size_gb >= 20
    error_message = "node_disk_size_gb must be at least 20 GB."
  }
}

variable "subnet_cidr" {
  description = "Primary node subnet range."
  type        = string
  default     = "10.10.0.0/20"
}

variable "pods_cidr" {
  description = "Secondary VPC-native Pod range."
  type        = string
  default     = "10.20.0.0/16"
}

variable "services_cidr" {
  description = "Secondary VPC-native Service range."
  type        = string
  default     = "10.30.0.0/20"
}

variable "master_authorized_networks" {
  description = "Non-empty CIDR allowlist for the public GKE control plane."
  type = list(object({
    cidr_block   = string
    display_name = string
  }))

  validation {
    condition = length(var.master_authorized_networks) > 0 && alltrue([
      for network in var.master_authorized_networks :
      can(cidrhost(network.cidr_block, 0)) && !contains(["0.0.0.0/0", "::/0"], network.cidr_block)
    ])
    error_message = "master_authorized_networks must contain valid CIDRs and must not allow the entire internet."
  }
}

variable "artifact_repository" {
  description = "Artifact Registry Docker repository name."
  type        = string
  default     = "agentops"
}

variable "mlflow_bucket_name" {
  description = "Globally unique MLflow artifact bucket; null derives it from project_id."
  type        = string
  default     = null
  nullable    = true
}

variable "deletion_protection" {
  description = "Protect the course cluster from accidental deletion."
  type        = bool
  default     = false
}

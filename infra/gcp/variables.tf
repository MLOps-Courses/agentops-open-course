variable "project_id" {
  description = "Existing, billing-enabled Google Cloud project."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "project_id must be a valid Google Cloud project ID."
  }
}

variable "region" {
  description = "Region for the VPC subnet and the container registry."
  type        = string
  default     = "europe-west1"

  validation {
    condition     = can(regex("^[a-z]+-[a-z]+[0-9]+$", var.region))
    error_message = "region must be a valid Google Cloud region such as europe-west1."
  }
}

variable "zone" {
  description = "Single GKE zone; a zonal cluster avoids multi-zone node cost."
  type        = string
  default     = "europe-west1-b"

  validation {
    condition = (
      can(regex("^[a-z]+-[a-z]+[0-9]+-[a-z]$", var.zone)) &&
      startswith(var.zone, "${var.region}-")
    )
    error_message = "zone must be a valid single zone inside region."
  }
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

  validation {
    condition     = var.machine_type == "e2-standard-2"
    error_message = "machine_type must remain e2-standard-2, the tested disposable-lab size."
  }
}

variable "spot_nodes" {
  description = "Use interruptible Spot capacity for the single course node."
  type        = bool
  default     = true

  validation {
    condition     = var.spot_nodes
    error_message = "spot_nodes must remain enabled for the disposable course lab."
  }
}

variable "node_disk_size_gb" {
  description = "Boot disk size for the single node."
  type        = number
  default     = 30

  validation {
    condition     = var.node_disk_size_gb >= 20 && var.node_disk_size_gb <= 30
    error_message = "node_disk_size_gb must stay between 20 and the tested 30 GB ceiling."
  }
}

variable "subnet_cidr" {
  description = "Primary node subnet range."
  type        = string
  default     = "10.10.0.0/20"

  validation {
    condition     = can(cidrhost(var.subnet_cidr, 0)) && can(regex("^[0-9.]+/[0-9]+$", var.subnet_cidr))
    error_message = "subnet_cidr must be a valid IPv4 CIDR."
  }
}

variable "pods_cidr" {
  description = "Secondary VPC-native Pod range."
  type        = string
  default     = "10.20.0.0/16"

  validation {
    condition     = can(cidrhost(var.pods_cidr, 0)) && can(regex("^[0-9.]+/[0-9]+$", var.pods_cidr))
    error_message = "pods_cidr must be a valid IPv4 CIDR."
  }
}

variable "services_cidr" {
  description = "Secondary VPC-native Service range."
  type        = string
  default     = "10.30.0.0/20"

  validation {
    condition = (
      can(cidrhost(var.services_cidr, 0)) &&
      can(regex("^[0-9.]+/[0-9]+$", var.services_cidr)) &&
      !try(cidrcontains(var.subnet_cidr, cidrhost(var.pods_cidr, 0)), true) &&
      !try(cidrcontains(var.pods_cidr, cidrhost(var.subnet_cidr, 0)), true) &&
      !try(cidrcontains(var.subnet_cidr, cidrhost(var.services_cidr, 0)), true) &&
      !try(cidrcontains(var.services_cidr, cidrhost(var.subnet_cidr, 0)), true) &&
      !try(cidrcontains(var.pods_cidr, cidrhost(var.services_cidr, 0)), true) &&
      !try(cidrcontains(var.services_cidr, cidrhost(var.pods_cidr, 0)), true)
    )
    error_message = "subnet_cidr, pods_cidr, and services_cidr must be valid, pairwise non-overlapping IPv4 ranges."
  }
}

variable "master_authorized_networks" {
  description = "Non-empty CIDR allowlist for the public GKE control plane."
  type = list(object({
    cidr_block   = string
    display_name = string
  }))

  validation {
    condition = length(var.master_authorized_networks) == 1 && alltrue([
      for network in var.master_authorized_networks :
      can(cidrhost(network.cidr_block, 0)) &&
      can(regex("^([0-9]{1,3}\\.){3}[0-9]{1,3}/32$", network.cidr_block)) &&
      trimspace(network.display_name) != "" &&
      !try(cidrcontains(var.subnet_cidr, cidrhost(network.cidr_block, 0)), true) &&
      !try(cidrcontains(var.pods_cidr, cidrhost(network.cidr_block, 0)), true) &&
      !try(cidrcontains(var.services_cidr, cidrhost(network.cidr_block, 0)), true)
    ])
    error_message = "master_authorized_networks must contain one named public IPv4 /32 outside every VPC range."
  }
}

variable "artifact_repository" {
  description = "Artifact Registry Docker repository name."
  type        = string
  default     = "agentops"
}

variable "deletion_protection" {
  description = "Protect the course cluster from accidental deletion."
  type        = bool
  default     = false

  validation {
    condition     = !var.deletion_protection
    error_message = "deletion_protection must remain disabled for the disposable course lab."
  }
}

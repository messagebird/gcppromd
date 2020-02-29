# gcppromd

GCPPromd provides Google Compute Engine auto-discovery for Prometheus.

## Configuration

```
Usage of ./gcppromd:
  -daemon
        run the application as a daemon that periodically produses a targets file with a json in Prometheus file_sd format. Disables web-mode
  -frequency int
        discovery frequency in seconds (default 300)
  -listen string
        HTTP listen address (default ":8080")
  -outputPath string
        A path to the outputfile with targets (default "/etc/prom_sd/targets.json")
  -projects string
        comma-separated projects IDs (default: auto discovery)

```

## Docker image

A docker image is [available](https://hub.docker.com/r/messagebird/gcppromd/).
```
docker run messagebird/gcppromd:latest
```

## API Reference

| Endpoint                                                 | Description    |
| -------------------------------------------------------- | -------------- |
| `GET /status`                                            | Health check   |
| `GET /v1/gce/instances?projects=<project1,project2,...>` | List instances |

### GCE instance discovery

#### Daemon mode

Outputs a JSON with Prometheus targets in projects (`-projects`) to a file set by `-outputPath`.

#### Web-server mode
The http request

`GET /v1/gce/instances?projects=<project1,project2,...>`

returns an array of prometheus's [`<static_config>`](https://prometheus.io/docs/prometheus/latest/configuration/configuration/#static_config)s.

The query parameter `projects` accepts a list of coma separated google cloud project names.

### General Notes (true for both web-server and daemon mode)
The instances on those projects that have the GCE label `prometheus` (the value doesn't matter) are returned.

Every instance can have one or multiple metadata keys, *be careful metadata are not labels*, of the form `prometheus_ports`
or `prometheus_ports_<service name>` mapping to the port number that prometheus
should target.

For every GCE metadata like `prometheus_ports_*` a couple of Prometheus targets and Prometheus labels are emitted.

An instance can also declare the metadata keys `prometheus_delegate_address` or
`prometheus_delegate_address_<service name>` with
`prometheus_delegate_ports` or `prometheus_delegate_ports_<service name>`.

These are useful for automatically discovering instances behind a load-balancer.

- `__meta_gce_instance_name`: the name of the instance
- `__meta_gce_metadata_`<name>: each metadata item of the instance
- `__meta_gce_network`: the network URL of the instance
- `__meta_gce_private_ip`: the private IP address of the instance
- `__meta_gce_project`: the GCP project in which the instance is running
- `__meta_gce_public_ip`: the public IP address of the instance, if present
- `__meta_gce_subnetwork`: the subnetwork URL of the instance
- `__meta_gce_tags`: comma separated list of instance tags
- `__meta_gce_zone`: the GCE zone URL in which the instance is running
- `__meta_gce_region`: the GCE region name in which the instance is running
- `__meta_gce_delagate_for_`: URLs of the instance delegate.
- `__meta_gce_name`: the extracted name from the `prometheus_port_*` GCE label, an empty string if the label is exactly `prometheus_port`

## Authentication

To authenticate with the google cloud APIs you can use the [Application Default Credentials process](https://cloud.google.com/docs/authentication/production) or set specific credentials using the `GOOGLE_APPLICATION_CREDENTIALS` environment variable.

Thse credentials need to have the API scope `https://www.googleapis.com/auth/compute.readonly`.

## Errors

No errors are ever returned by the API. They are only logged.

## FAQ
### In what this is different than [`gce_sd_configs`](https://prometheus.io/docs/prometheus/latest/configuration/configuration/#%3Cgce_sd_config%3E)?

Built in support for GCE in prometheus requires you to manually declare all your projects and zones. This can be tedious when you need to map many Google projects across multiple zones.

### How do I pass the discovered instances to prometheus?

GCPPromd returns an array of `<static_config>`, you can automatically write that output to a file and use [`<file_sd_config>`]( https://prometheus.io/docs/prometheus/latest/configuration/configuration/#%3Cfile_sd_config%3E).
Prometheus will reload the configuration automatically.

### I don't see any labels?
All emitted labels are prefixed with  `__meta` you need to explicitly relabel to pick what you need.
For example, assuming that the output of `GET /v1/gce/instances` is written in `/var/local/gcppromd/gcppromd_v1_gce_instances.json`:

```yaml
- job_name: 'gce_auto_discovery'

  file_sd_configs:
    - files:
        - /var/local/gcppromd/gcppromd_v1_gce_instances.json
      refresh_interval: 10m

  relabel_configs:
    - source_labels: ['__meta_gce_name']
      regex: '(.*)'
      target_label: 'gce_name'
      replacement: '$1'
    - source_labels: ['__meta_gce_instance_name']
      regex: '(.*)'
      target_label: 'instance'
      replacement: '$1'
    - source_labels: ['__meta_gce_label_my_label_name']
      regex: '(.*)'
      target_label: 'my_label_name'
      replacement: '$1'
```



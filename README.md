# gcppromd

GCPPromd provides Google Compute Engine  auto-discovery for Prometheus.

## Configuration

```
Usage of gcppromd:
  -listen string
    	HTTP listen address (default ":8080")
```
The authentication against Google APIs is done using [Application Default Credentials process](https://cloud.google.com/docs/authentication/production).  
You can set specific credential with the environment variable `GOOGLE_APPLICATION_CREDENTIALS`.

## API Reference

| Endpoint                                                 | Description    |
| -------------------------------------------------------- | -------------- |
| `GET /status`                                            | Health check   |
| `GET /v1/gce/instances?projects=<project1,project2,...>` | List instances |

### GCE instances discovery

`GET /v1/gce/instances?projects=<project1,project2,...>`

Accepts a query string parameter `projects` with the projects to scan for
discovering prometheus targets.
The Google credential used by the gcppromd requires the scope `https://www.googleapis.com/auth/compute.readonly` .


It returns an array of prometheus [`<static_config>`](https://prometheus.io/docs/prometheus/latest/configuration/configuration/#static_config).  
Only the instances with the GCE label `prometheus` (the value don't matter) are
returned.

Every instance can have one or multiple metadata keys, *be careful metadata are not labels*, of the form `prometheus_ports`
or `prometheus_ports_<service name>` mapping to the port number that prometheus
should target.

For every GCE label `prometheus_ports_*` a couple of targets and labels are emitted

An instance can also declare the metadata keys `prometheus_delegate_address` or
`prometheus_delegate_address_<service name>` with
`prometheus_delegate_ports` or `prometheus_delegate_ports_<service name>`.
Useful for automatically discover instances behind a load-balancer.

- `__meta_gce_instance_name`: the name of the instance
- `__meta_gce_metadata_`<name>: each metadata item of the instance
- `__meta_gce_network`: the network URL of the instance
- `__meta_gce_private_ip`: the private IP address of the instance
- `__meta_gce_project`: the GCP project in which the instance is running
- `__meta_gce_public_ip`: the public IP address of the instance, if present
- `__meta_gce_subnetwork`: the subnetwork URL of the instance
- `__meta_gce_tags`: comma separated list of instance tags
- `__meta_gce_zone`: the GCE zone URL in which the instance is running
- `__meta_gce_delagate_for_`: URLs of the instance delegate.
- `__meta_gce_name`: the extracted name from the `prometheus_port_*` GCE label, an empty string if the label is exactly `prometheus_port`

## Errors

Returns no errors, if any happens it can only be seen in logs.

## FAQ
### In what this is different of [`gce_sd_configs`](https://prometheus.io/docs/prometheus/latest/configuration/configuration/#%3Cgce_sd_config%3E)?

Built in support for GCE in prometheus requires you to manually declare all your projects and zones. This can be tedious when you need to map many Goole projects across multiple zones.

### How do I pass the discovered instances to prometheus?

GCPPromd returns an array of `<static_config>`, you can automatically write that output to a file and use [`<file_sd_config>`]( https://prometheus.io/docs/prometheus/latest/configuration/configuration/#%3Cfile_sd_config%3E).  
Prometheus will reload the configuration automatically.

### I don't see any labels?
All the emitted labels are prefixed with  `__meta` you need to explicitly relabel to pick what you need.  
For example, assuming that the output of `GET /v1/gce/instances` is written in `/var/local/gcppromd/gcppromd_v1_gce_instances.json`:

```
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

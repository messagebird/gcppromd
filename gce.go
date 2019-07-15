package gcppromd

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/prometheus/common/model"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/compute/v1"

	pmodel "github.com/prometheus/common/model"
	pstrutil "github.com/prometheus/prometheus/util/strutil"
	"github.com/pkg/errors"
)

const (
	// Label names generated
	promLabel                 = model.MetaLabelPrefix + "gce_"
	promLabelProject          = promLabel + "project"
	promLabelZone             = promLabel + "zone"
	promLabelRegion           = promLabel + "region"
	promLabelNetwork          = promLabel + "network"
	promLabelSubnetwork       = promLabel + "subnetwork"
	promLabelPublicIP         = promLabel + "public_ip"
	promLabelPrivateIP        = promLabel + "private_ip"
	promLabelInstanceName     = promLabel + "instance_name"
	promLabelInstanceStatus   = promLabel + "instance_status"
	promLabelTags             = promLabel + "tags"
	promLabelMetadata         = promLabel + "metadata_"
	promLabelName             = promLabel + "name"
	promLabelLabel            = promLabel + "label_"
	promLabelDelegateForNames = promLabel + "delegate_for_instances"
	// separator used to join the GCE tag in one prom label or to declare
	// multiple ports in the same gce metadata
	promSeparator = ","
	// filter to use to identify the instances scrapable by prometheus
	promPresenceFilter = "(labels.prometheus eq .*)"
	// Label prefixes scraped from GCE instance labels
	gcePrefix              = "prometheus_"
	gcePrefixPorts         = gcePrefix + "ports_"
	gcePrefixDelegateAddr  = gcePrefix + "delegate_address_"
	gcePrefixDelegatePorts = gcePrefix + "delegate_ports_"
)

// GCEReqInstanceDiscovery work unit for a pool of GCEDiscovery workers
type GCEReqInstanceDiscovery struct {
	Project           string
	Filter            string
	PrometheusConfigs chan []*PromConfig
	Errors            chan error
}

// GCEDiscovery represents a Google Compute Engine discovery configuration for one Google project.
type GCEDiscovery struct {
	service *compute.Service
}

type delagatedHost struct {
	address     string
	ports       []int
	delegateFor []string
}

// NewGCEDiscoveryPool creates a pool <size> go routine to process the discovery requests in parallel.
func NewGCEDiscoveryPool(ctx context.Context, size int) (chan *GCEReqInstanceDiscovery, error) {
	gced, err := NewGCEDiscovery()
	if err != nil {
		return nil, err
	}

	reqs := make(chan *GCEReqInstanceDiscovery)
	for n := 0; n < size; n++ {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case req := <-reqs:
					confs, err := gced.Instances(ctx, req.Project, req.Filter)
					if err != nil {
						req.Errors <- err
					} else {
						req.PrometheusConfigs <- confs
					}
				}
			}
		}()
	}
	return reqs, nil
}

// NewGCEDiscovery returns an object of GCEDiscovery type.
func NewGCEDiscovery() (*GCEDiscovery, error) {
	ctx := context.Background()
	cl, err := google.DefaultClient(ctx, compute.CloudPlatformScope)
	if err != nil {
		return nil, err
	}

	service, err := compute.New(cl)
	if err != nil {
		return nil, err
	}

	d := GCEDiscovery{service: service}

	return &d, nil
}

// Instances returns a list of instances of a directory project.
func (d *GCEDiscovery) Instances(ctx context.Context, project, filter string) ([]*PromConfig, error) {
	configs := make([]*PromConfig, 0, 100)
	ialReq := d.service.Instances.
		AggregatedList(project).
		Fields(
			"nextPageToken",
			"items/*/instances(id,status,zone,name,tags,labels,networkInterfaces,selfLink,metadata)",
		)

	filter = strings.Trim(promPresenceFilter+" ", " ")
	if filter != "" {
		ialReq = ialReq.Filter(filter)
	}

	delagatedHosts := make(map[string]*delagatedHost)
	regions, err := d.regions(ctx, project)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	err = ialReq.Pages(ctx, func(ial *compute.InstanceAggregatedList) error {
		for _, zone := range ial.Items {
			for _, inst := range zone.Instances {
				if len(inst.NetworkInterfaces) <= 0 {
					continue
				}

				priIface := inst.NetworkInterfaces[0]

				region, err := extractRegionFromZone(inst.Zone, regions)
				if err != nil {
					return errors.Wrapf(err, "could not determine in which region instance %s is.", inst.Name)
				}

				labels := pmodel.LabelSet{
					promLabelProject:        pmodel.LabelValue(project),
					promLabelZone:           pmodel.LabelValue(inst.Zone),
					promLabelRegion:         pmodel.LabelValue(region),
					promLabelInstanceName:   pmodel.LabelValue(inst.Name),
					promLabelInstanceStatus: pmodel.LabelValue(inst.Status),
					promLabelNetwork:        pmodel.LabelValue(priIface.Network),
					promLabelSubnetwork:     pmodel.LabelValue(priIface.Subnetwork),
					promLabelPrivateIP:      pmodel.LabelValue(priIface.NetworkIP),
				}

				if len(priIface.AccessConfigs) > 0 {
					ac := priIface.AccessConfigs[0]
					if ac.Type == "ONE_TO_ONE_NAT" {
						labels[promLabelPublicIP] = pmodel.LabelValue(ac.NatIP)
					}
				}

				if inst.Tags != nil && len(inst.Tags.Items) > 0 {
					// We surround the separated list with the separator as well. This way regular expressions
					// in relabeling rules don't have to consider tag positions.
					tags := promSeparator + strings.Join(inst.Tags.Items, promSeparator) + promSeparator
					labels[promLabelTags] = pmodel.LabelValue(tags)
				}

				if inst.Labels != nil {
					for key, v := range inst.Labels {
						name := pstrutil.SanitizeLabelName(key)
						labels[promLabelLabel+model.LabelName(name)] = model.LabelValue(v)
					}
				}

				// GCE metadata are key-value pairs for user supplied attributes.
				if inst.Metadata != nil {
					// keep track of the localy created label set.
					lTargetsLabels := make([]pmodel.LabelSet, 0)
					// this loop do not populates the __meta_gce_metadata_...
					// labels only generate the different targets.
					for _, i := range inst.Metadata.Items {
						// Protect against occasional nil pointers.
						if i.Value == nil {
							continue
						}
						key, v := i.Key, *i.Value

						paddedKey := key + "_" // pad the key with _ to match naked prefix
						if ports, name, ok := parsePorts(paddedKey, v, gcePrefixPorts); ok {
							addrs := make([]string, 0, len(ports))
							for _, port := range ports {
								addr := fmt.Sprintf("%s:%d", priIface.NetworkIP, port)
								addrs = append(addrs, addr)
							}
							targetLabels := labels.Clone()
							targetLabels[model.LabelName(promLabelName)] = model.LabelValue(name)
							pc := &PromConfig{addrs, targetLabels}
							configs = append(configs, pc)
							lTargetsLabels = append(lTargetsLabels, targetLabels)
							continue
						}

						if ports, name, ok := parsePorts(paddedKey, v, gcePrefixDelegatePorts); ok {
							if _, ok := delagatedHosts[name]; !ok {
								delagatedHosts[name] = &delagatedHost{}
							}
							delagatedHosts[name].ports = ports
							continue
						}

						if strings.HasPrefix(paddedKey, gcePrefixDelegateAddr) {
							name := parseNameFromKey(paddedKey, gcePrefixDelegateAddr)
							if _, ok := delagatedHosts[name]; !ok {
								delagatedHosts[name] = &delagatedHost{}
							}
							delagatedHosts[name].address = v
							delagatedHosts[name].delegateFor = append(
								delagatedHosts[name].delegateFor,
								inst.SelfLink,
							)
							continue
						}
					}
					// populates cloned labels wiht the __meta_gce_metadata
					for _, i := range inst.Metadata.Items {
						if i.Value == nil {
							continue
						}
						key, v := i.Key, *i.Value
						for _, tlabels := range lTargetsLabels {
							name := pstrutil.SanitizeLabelName(key)
							tlabels[promLabelMetadata+model.LabelName(name)] = model.LabelValue(v)
						}
					}
				}
			}
		}
		return nil
	})

	// nolabels := pmodel.LabelSet{}
	for name, delegated := range delagatedHosts {
		tags := promSeparator + strings.Join(delegated.delegateFor, promSeparator) + promSeparator
		largetLables := pmodel.LabelSet{
			promLabelDelegateForNames:      pmodel.LabelValue(tags),
			model.LabelName(promLabelName): pmodel.LabelValue(name),
		}

		for _, port := range delegated.ports {
			addr := []string{fmt.Sprintf("%s:%d", delegated.address, port)}
			fmt.Println(addr)
			pc := &PromConfig{addr, largetLables}
			configs = append(configs, pc)
		}
	}

	return configs, err
}

func (d *GCEDiscovery) regions(ctx context.Context, project string) (map[string]string, error) {
	m := make(map[string]string)

	req := d.service.Regions.List(project)
	err := req.Pages(context.Background(), func(list *compute.RegionList) error {
		for _, r := range list.Items {
			m[r.Name] = r.Name
		}
		return nil
	})
	if err != nil {
		return nil, errors.Wrap(err, "could not retrieve/process list of GCP regions.")
	}

	return m, nil
}

func parsePorts(key, value, prefix string) (ports []int, name string, has bool) {
	has = strings.HasPrefix(key, prefix)
	if !has {
		return
	}
	name = parseNameFromKey(key, prefix)
	pvalues := strings.Split(value, promSeparator)
	for _, pv := range pvalues {
		port, err := strconv.Atoi(pv)
		if err != nil {
			// ignore invalid port
			continue
		}
		ports = append(ports, port)
	}
	return
}

func parseNameFromKey(key, prefix string) (name string) {
	plen := len(prefix)
	if len(key) > plen {
		// non-naked prefix extract name
		name = strings.TrimRight(key[plen:], "_")
	}
	return
}

func extractRegionFromZone(zoneURL string, regions map[string]string) (string, error) {
	urlSplit := strings.Split(zoneURL, "/")
	zone := urlSplit[len(urlSplit) - 1]

	if val, ok := regions[zone[:len(zone) - 2]]; ok {
		return val, nil
	}

	return "", errors.New(fmt.Sprintf("could not find region for zone %s", zone))
}

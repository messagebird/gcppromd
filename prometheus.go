package gcppromd

import pmodel "github.com/prometheus/common/model"

// PromConfig a definition of a <static_config> in prometheus https://prometheus.io/docs/prometheus/latest/configuration/configuration/#static_config
type PromConfig struct {
	Targets []string        `json:"targets"`
	Labels  pmodel.LabelSet `json:"labels"`
}

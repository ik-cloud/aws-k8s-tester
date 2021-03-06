// Copied from https://github.com/kubernetes/autoscaler/blob/master/cluster-autoscaler/cloudprovider/aws/ec2_instance_types/gen.go

/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// instance-types auto-generates EC2 instance types from AWS API.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/aws/aws-k8s-tester/pkg/httputil"

	"github.com/aws/aws-sdk-go/aws/endpoints"
	"go.uber.org/zap"
)

var lg *zap.Logger

func init() {
	var err error
	lg, err = zap.NewProduction()
	if err != nil {
		panic(err)
	}
}

type response struct {
	Products map[string]product `json:"products"`
}

type product struct {
	Attributes productAttributes `json:"attributes"`
}

type productAttributes struct {
	InstanceType string `json:"instanceType"`
	VCPU         string `json:"vcpu"`
	Memory       string `json:"memory"`
	GPU          string `json:"gpu"`
}

type instanceType struct {
	InstanceType string
	VCPU         int64
	Memory       int64
	GPU          int64
	MaxPods      int64
}

var (
	tmpl = fmt.Sprintf(`/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// This file was generated by go generate; DO NOT EDIT

// generated at %v

package ec2

// InstanceType is an EC2 instance type.
type InstanceType struct {
	InstanceType string
	VCPU         int64
	MemoryMb     int64
	GPU          int64
	MaxPods      int64
}

// InstanceTypes is a map of EC2 resources.
var InstanceTypes = map[string]*InstanceType{
{{- range .InstanceTypes }}
	"{{ .InstanceType }}": {
		InstanceType: "{{ .InstanceType }}",
		VCPU:         {{ .VCPU }},
		MemoryMb:     {{ .Memory }},
		GPU:          {{ .GPU }},
		MaxPods:      {{ .MaxPods }},
	},
{{- end }}
}
`, time.Now().UTC())

	pkgTmpl = template.Must(template.New("").Parse(tmpl))
)

func main() {
	maxPodsData, merr := httputil.Download(lg, os.Stdout, "https://raw.githubusercontent.com/awslabs/amazon-eks-ami/master/files/eni-max-pods.txt")
	if merr != nil {
		lg.Fatal("failed to download ENI max pods", zap.Error(merr))
	}
	instanceToMaxPods := make(map[string]int64)
	for _, line := range strings.Split(string(maxPodsData), "\n") {
		if strings.HasPrefix(line, "# ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			lg.Warn("skipping line", zap.String("line", line))
			continue
		}
		n, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			lg.Fatal("failed to parse int", zap.Error(err))
		}
		instanceToMaxPods[fields[0]] = n
	}

	instanceTypes := make(map[string]*instanceType)

	resolver := endpoints.DefaultResolver()
	partitions := resolver.(endpoints.EnumPartitions).Partitions()
	lg.Info("partitions", zap.Int("total", len(partitions)))
	for _, p := range partitions {
		lg.Info("partition", zap.String("id", p.ID()), zap.Int("total-regions", len(p.Regions())))
		for _, r := range p.Regions() {
			u := "https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonEC2/current/" + r.ID() + "/index.json"
			d, err := httputil.Download(lg, os.Stdout, u)
			if err != nil {
				lg.Warn("failed to download", zap.String("url", u), zap.String("data", string(d)), zap.Error(err))
				continue
			}

			var rs response
			err = json.Unmarshal(d, &rs)
			if err != nil {
				if len(d) < 250 {
					lg.Warn("contents are missing?", zap.String("url", u), zap.String("data", string(d)), zap.Error(err))
					continue
				}
				lg.Warn("failed to unmarshal", zap.String("url", u), zap.String("data", string(d)), zap.Error(err))
				continue
			}

			for _, product := range rs.Products {
				attr := product.Attributes
				if attr.InstanceType != "" {
					instanceTypes[attr.InstanceType] = &instanceType{
						InstanceType: attr.InstanceType,
					}
					if attr.Memory != "" && attr.Memory != "NA" {
						instanceTypes[attr.InstanceType].Memory = parseMemory(attr.Memory)
					}
					if attr.VCPU != "" {
						instanceTypes[attr.InstanceType].VCPU = parseCPU(attr.VCPU)
					}
					if attr.GPU != "" {
						instanceTypes[attr.InstanceType].GPU = parseCPU(attr.GPU)
					}
					if !strings.Contains(attr.InstanceType, ".") {
						continue
					}
					v, ok := instanceToMaxPods[attr.InstanceType]
					if !ok {
						lg.Warn("failed to find max pods", zap.String("instance-type", attr.InstanceType))
					}
					instanceTypes[attr.InstanceType].MaxPods = v
				}
			}
		}
	}

	f, err := os.Create("instance_types.go")
	if err != nil {
		lg.Fatal("failed to write 'instance_types.go'", zap.Error(err))
	}
	defer f.Close()

	if err = pkgTmpl.Execute(f, struct {
		InstanceTypes map[string]*instanceType
	}{
		InstanceTypes: instanceTypes,
	}); err != nil {
		lg.Fatal("failed to write template", zap.Error(err))
	}

	if err := exec.Command("go", "fmt", "instance_types.go").Run(); err != nil {
		lg.Fatal("failed to 'gofmt'", zap.Error(err))
	}

	lg.Info("done!")
}

func parseMemory(memory string) int64 {
	reg, err := regexp.Compile("[^0-9\\.]+")
	if err != nil {
		lg.Fatal("failed to compile regex", zap.Error(err))
	}

	parsed := strings.TrimSpace(reg.ReplaceAllString(memory, ""))
	mem, err := strconv.ParseFloat(parsed, 64)
	if err != nil {
		lg.Fatal("failed to parse float", zap.Error(err))
	}

	return int64(mem * float64(1024))
}

func parseCPU(cpu string) int64 {
	i, err := strconv.ParseInt(cpu, 10, 64)
	if err != nil {
		lg.Fatal("failed to parse integer", zap.Error(err))
	}
	return i
}

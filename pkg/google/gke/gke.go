package gke

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	billingv1 "cloud.google.com/go/billing/apiv1"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/api/compute/v1"

	"github.com/grafana/cloudcost-exporter/pkg/google/billing"
	gcpCompute "github.com/grafana/cloudcost-exporter/pkg/google/compute"

	cloudcostexporter "github.com/grafana/cloudcost-exporter"
	"github.com/grafana/cloudcost-exporter/pkg/provider"
)

const (
	subsystem = "gcp_gke"
)

var (
	gkeNodeMemoryHourlyCostDesc = prometheus.NewDesc(
		prometheus.BuildFQName(cloudcostexporter.MetricPrefix, subsystem, "instance_memory_usd_per_gib_hour"),

		"The cpu cost a GKE Instance in USD/(core*h)",
		// Cannot simply do cluster because many metric scrapers will add a label for cluster and would interfere with the label we want to add
		[]string{"cluster_name", "instance", "region", "family", "machine_type", "project", "price_tier"},
		nil,
	)
	gkeNodeCPUHourlyCostDesc = prometheus.NewDesc(
		prometheus.BuildFQName(cloudcostexporter.MetricPrefix, subsystem, "instance_cpu_usd_per_core_hour"),
		"The memory cost of a GKE Instance in USD/(GiB*h)",
		// Cannot simply do cluster because many metric scrapers will add a label for cluster and would interfere with the label we want to add
		[]string{"cluster_name", "instance", "region", "family", "machine_type", "project", "price_tier"},
		nil,
	)
	persistentVolumeHourlyCostDesc = prometheus.NewDesc(
		prometheus.BuildFQName(cloudcostexporter.MetricPrefix, subsystem, "persistent_volume_usd_per_gib_hour"),
		"The cost of a GKE Persistent Volume in USD/(GiB*h)",
		[]string{"cluster_name", "namespace", "persistentvolume", "region", "project", "storage_class"},
		nil,
	)
)

var (
	keys = []string{"kubernetes.io/created-for/pvc/namespace", "kubernetes.io-created-for/pvc-namespace"}
)

type Config struct {
	Projects       string
	ScrapeInterval time.Duration
}

type Collector struct {
	computeService    *compute.Service
	billingService    *billingv1.CloudCatalogClient
	config            *Config
	Projects          []string
	ComputePricingMap *gcpCompute.StructuredPricingMap
	NextScrape        time.Time
}

func (c *Collector) Register(_ provider.Registry) error {
	return nil
}

func (c *Collector) CollectMetrics(ch chan<- prometheus.Metric) float64 {
	err := c.Collect(ch)
	if err != nil {
		log.Printf("failed to collect metrics: %v", err)
		return 0
	}
	return 1
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) error {
	ctx := context.TODO()
	if c.ComputePricingMap == nil || time.Now().After(c.NextScrape) {
		serviceName, err := billing.GetServiceName(ctx, c.billingService, "Compute Engine")
		if err != nil {
			return err
		}
		skus := billing.GetPricing(ctx, c.billingService, serviceName)
		c.ComputePricingMap, err = gcpCompute.GeneratePricingMap(skus)
		if err != nil {
			return err
		}
		c.NextScrape = time.Now().Add(c.config.ScrapeInterval)
	}

	for _, project := range c.Projects {
		zones, err := c.computeService.Zones.List(project).Do()
		if err != nil {
			return err
		}
		wg := sync.WaitGroup{}
		// Multiply by 2 because we are making two requests per zone
		wg.Add(len(zones.Items) * 2)
		instances := make(chan []*gcpCompute.MachineSpec, len(zones.Items))
		disks := make(chan []*compute.Disk, len(zones.Items))
		for _, zone := range zones.Items {
			go func(zone *compute.Zone) {
				defer wg.Done()
				results, err := gcpCompute.ListInstancesInZone(project, zone.Name, c.computeService)
				if err != nil {
					log.Printf("error listing instances in zone %s: %v", zone.Name, err)
					instances <- nil
					return
				}
				instances <- results
			}(zone)
			go func(zone *compute.Zone) {
				defer wg.Done()
				results, err := ListDisks(project, zone.Name, c.computeService)
				if err != nil {
					log.Printf("error listing disks in zone %s: %v", zone.Name, err)
					return
				}
				disks <- results
			}(zone)
		}

		go func() {
			wg.Wait()
			close(instances)
			close(disks)
		}()

		for group := range instances {
			if instances == nil {
				continue
			}
			for _, instance := range group {
				clusterName := instance.GetClusterName()
				// We skip instances that do not have a clusterName because they are not associated with an GKE cluster
				if clusterName == "" {
					continue
				}
				labelValues := []string{
					clusterName,
					instance.Instance,
					instance.Region,
					instance.Family,
					instance.MachineType,
					project,
					instance.PriceTier,
				}
				cpuCost, ramCost, err := c.ComputePricingMap.GetCostOfInstance(instance)
				if err != nil {
					return err
				}
				ch <- prometheus.MustNewConstMetric(
					gkeNodeCPUHourlyCostDesc,
					prometheus.GaugeValue,
					cpuCost,
					labelValues...,
				)
				ch <- prometheus.MustNewConstMetric(
					gkeNodeMemoryHourlyCostDesc,
					prometheus.GaugeValue,
					ramCost,
					labelValues...,
				)
			}
		}
		seenDisks := make(map[string]bool)
		for group := range disks {
			for _, disk := range group {
				clusterName := disk.Labels[gcpCompute.GkeClusterLabel]
				region := getRegionFromDisk(disk)

				namespace := getNamespaceFromDisk(disk)
				name := getNameFromDisk(disk)
				if _, ok := seenDisks[name]; ok {
					continue
				}
				seenDisks[name] = true
				diskType := strings.Split(disk.Type, "/")
				storageClass := diskType[len(diskType)-1]
				labelValues := []string{
					clusterName,
					namespace,
					name,
					region,
					project,
					storageClass,
				}

				price, err := c.ComputePricingMap.GetCostOfStorage(region, storageClass)
				if err != nil {

					fmt.Printf("%s error getting cost of storage: %v\n", disk.Name, err)
					continue
				}
				ch <- prometheus.MustNewConstMetric(
					persistentVolumeHourlyCostDesc,
					prometheus.GaugeValue,
					float64(disk.SizeGb)*price,
					labelValues...,
				)
			}
		}
	}
	return nil
}

func New(config *Config, computeService *compute.Service, billingService *billingv1.CloudCatalogClient) *Collector {
	projects := strings.Split(config.Projects, ",")
	return &Collector{
		computeService: computeService,
		billingService: billingService,
		config:         config,
		Projects:       projects,
	}
}

func (c *Collector) Name() string {
	return subsystem
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) error {
	ch <- gkeNodeCPUHourlyCostDesc
	ch <- gkeNodeMemoryHourlyCostDesc
	return nil
}

// ListDisks will list all disks in a given zone and return a slice of compute.Disk
func ListDisks(project string, zone string, service *compute.Service) ([]*compute.Disk, error) {
	var disks []*compute.Disk
	// TODO: How do we get this to work for multi regional disks?
	err := service.Disks.List(project, zone).Pages(context.Background(), func(page *compute.DiskList) error {
		if page == nil {
			return nil
		}
		disks = append(disks, page.Items...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return disks, nil
}

func getNamespaceFromDisk(disk *compute.Disk) string {
	desc := make(map[string]string)
	err := extractLabelsFromDesc(disk.Description, desc)
	if err != nil {
		return ""
	}
	return coalesce(desc, keys...)
}

func getRegionFromDisk(disk *compute.Disk) string {
	zone := disk.Labels[gcpCompute.GkeRegionLabel]
	if zone == "" {
		// This would be a case where the disk is no longer mounted _or_ the disk is associated with a Compute instance
		zone = disk.Zone[strings.LastIndex(disk.Zone, "/")+1:]
	}
	// If zone _still_ is empty we can't determine the region, so we return an empty string
	// This prevents an index out of bounds error
	if zone == "" {
		return ""
	}
	if strings.Count(zone, "-") < 2 {
		return zone
	}
	return zone[:strings.LastIndex(zone, "-")]
}

// getNameFromDisk will return the name of the disk. If the disk has a label "kubernetes.io/created-for/pv/name" it will return the value stored in that key.
// otherwise it will return the disk name that is directly associated with the disk.
func getNameFromDisk(disk *compute.Disk) string {
	desc := make(map[string]string)
	err := extractLabelsFromDesc(disk.Description, desc)
	if err != nil {
		return disk.Name
	}
	// first check that the key exists in the map, if it does return the value
	name := coalesce(desc, "kubernetes.io/created-for/pv/name", "kubernetes.io-created-for/pv-name")
	if name != "" {
		return name
	}
	return disk.Name
}

// coalesce will take a map and a list of keys and return the first value that is found in the map. If no value is found it will return an empty string
func coalesce(desc map[string]string, keys ...string) string {
	for _, key := range keys {
		if val, ok := desc[key]; ok {
			return val
		}
	}
	return ""
}

// extractLabelsFromDesc will take a description string and extract the labels from it. GKE disks store their description as
// a json blob in the description field. This function will extract the labels from that json blob and return them as a map
// Some useful information about the json blob are name, cluster, namespace, and pvc's that the disk is associated with
func extractLabelsFromDesc(description string, labels map[string]string) error {
	if description == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(description), &labels); err != nil {
		return err
	}
	return nil
}

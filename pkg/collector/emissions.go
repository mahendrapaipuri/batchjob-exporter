//go:build !noemissions
// +build !noemissions

package collector

import (
	"net/http"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/mahendrapaipuri/batchjob_monitoring/pkg/emissions"
)

const emissionsCollectorSubsystem = "emissions"

type emissionsCollector struct {
	logger              log.Logger
	countryCode         string
	energyData          map[string]float64
	emissionsMetricDesc *prometheus.Desc
	prevReadTime        int64
	prevEmissionFactor  float64
}

var (
	countryCode = kingpin.Flag(
		"collector.emissions.country.code",
		`ISO 3166-1 alpha-3 Country code. OWID energy data [https://github.com/owid/energy-data] 
estimated constant emission factor is used for all countries except for France. 
A real time emission factor will be used for France from RTE eCO2 mix 
[https://www.rte-france.com/en/eco2mix/co2-emissions] data.`,
	).Default("FRA").String()
	globalEmissionFactor = emissions.GlobalEmissionFactor
	getRteEnergyMixData  = emissions.GetRteEnergyMixEmissionData
)

func init() {
	registerCollector(emissionsCollectorSubsystem, defaultDisabled, NewEmissionsCollector)
}

// NewEmissionsCollector returns a new Collector exposing emission factor metrics.
func NewEmissionsCollector(logger log.Logger) (Collector, error) {
	energyData, err := emissions.GetEnergyMixData(http.DefaultClient, logger)
	if err != nil {
		level.Error(logger).Log("msg", "Failed to read Global energy mix data", "err", err)
	}
	level.Debug(logger).Log("msg", "Global energy mix data read successfully")

	emissionsMetricDesc := prometheus.NewDesc(
		prometheus.BuildFQName(namespace, emissionsCollectorSubsystem, "gCo2_kWh"),
		"Current eCO2 emissions in grams per kWh", []string{}, nil,
	)

	collector := emissionsCollector{
		logger:              logger,
		countryCode:         *countryCode,
		energyData:          energyData,
		emissionsMetricDesc: emissionsMetricDesc,
		prevReadTime:        time.Now().Unix(),
		prevEmissionFactor:  -1,
	}
	return &collector, nil
}

// Update implements Collector and exposes emission factor.
func (c *emissionsCollector) Update(ch chan<- prometheus.Metric) error {
	currentEmissionFactor := c.getCurrentEmissionFactor()
	// Returned value negative == emissions factor is not avail
	if currentEmissionFactor > -1 {
		ch <- prometheus.MustNewConstMetric(c.emissionsMetricDesc, prometheus.GaugeValue, float64(currentEmissionFactor))
	}
	return nil
}

// Get current emission factor
func (c *emissionsCollector) getCurrentEmissionFactor() float64 {
	// If country is other than france get factor from dataset
	if c.countryCode != "FRA" {
		if emissionFactor, ok := c.energyData[c.countryCode]; ok {
			level.Debug(c.logger).
				Log("msg", "Using emission factor from global energy data mix", "factor", emissionFactor)
			return emissionFactor
		} else {
			level.Debug(c.logger).Log("msg", "Using global average emission factor", "factor", globalEmissionFactor)
			return float64(globalEmissionFactor)
		}
	}
	return c.getCachedEmissionFactorFrance()
}

// Cache realtime emission factor and return cached value
// RTE updates data only for every hour. We make requests to RTE only once every 30 min
// and cache data for rest of the scrapes
func (c *emissionsCollector) getCachedEmissionFactorFrance() float64 {
	if time.Now().Unix()-c.prevReadTime > 1800 || c.prevEmissionFactor == -1 {
		currentEmissionFactor := c.getCurrentEmissionFactorFrance()
		c.prevReadTime = time.Now().Unix()
		c.prevEmissionFactor = currentEmissionFactor
		level.Debug(c.logger).
			Log("msg", "Using real time emission factor from RTE", "factor", currentEmissionFactor)
		return currentEmissionFactor
	} else {
		level.Debug(c.logger).Log("msg", "Using cached emission factor from previous request", "factor", c.prevEmissionFactor)
		return c.prevEmissionFactor
	}
}

// Get current emission factor for France from RTE energy data mix
func (c *emissionsCollector) getCurrentEmissionFactorFrance() float64 {
	emissionFactor, err := getRteEnergyMixData(http.DefaultClient, c.logger)
	if err != nil {
		level.Error(c.logger).Log("msg", "Failed to get emissions from RTE", "err", err)
		if emissionFactor, ok := c.energyData["FRA"]; ok {
			level.Debug(c.logger).
				Log("msg", "Using emissions from global energy data mix", "factor", emissionFactor)
			return emissionFactor
		} else {
			level.Debug(c.logger).Log("msg", "Using global average emissions factor", "factor", globalEmissionFactor)
			return float64(globalEmissionFactor)
		}
	}
	level.Debug(c.logger).
		Log("msg", "Current emission factor returned by RTE eCO2mix", "factor", emissionFactor)
	return emissionFactor
}

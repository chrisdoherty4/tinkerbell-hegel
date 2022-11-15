package handler

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"strings"

	"github.com/itchyny/gojq"
	"github.com/packethost/pkg/log"
	"github.com/pkg/errors"
	"github.com/tinkerbell/hegel/internal/datamodel"
	"github.com/tinkerbell/hegel/internal/hardware"
	"github.com/tinkerbell/hegel/internal/metrics"
)

// ec2Filters defines the query pattern and filters for the EC2 endpoint
// for queries that are to return another list of metadata items, the filter is a static list of the metadata items ("directory-listing filter")
// for /meta-data, the `spot` metadata item will only show up when the instance is a spot instance (denoted by if the `spot` field inside hardware is nonnull)
// NOTE: make sure when adding a new metadata item in a "subdirectory", to also add it to the directory-listing filter.
var ec2Filters = map[string]string{
	"":                                    `"meta-data", "user-data"`, // base path
	"/user-data":                          ".metadata.userdata",
	"/meta-data":                          `["instance-id", "hostname", "local-hostname", "iqn", "plan", "facility", "tags", "operating-system", "public-keys", "public-ipv4", "public-ipv6", "local-ipv4"] + (if .metadata.instance.spot != null then ["spot"] else [] end) | sort | .[]`,
	"/meta-data/instance-id":              ".metadata.instance.id",
	"/meta-data/hostname":                 ".metadata.instance.hostname",
	"/meta-data/local-hostname":           ".metadata.instance.hostname",
	"/meta-data/iqn":                      ".metadata.instance.iqn",
	"/meta-data/plan":                     ".metadata.instance.plan",
	"/meta-data/facility":                 ".metadata.instance.facility",
	"/meta-data/tags":                     ".metadata.instance.tags[]?",
	"/meta-data/operating-system":         `["slug", "distro", "version", "license_activation", "image_tag"] | sort | .[]`,
	"/meta-data/operating-system/slug":    ".metadata.instance.operating_system.slug",
	"/meta-data/operating-system/distro":  ".metadata.instance.operating_system.distro",
	"/meta-data/operating-system/version": ".metadata.instance.operating_system.version",
	"/meta-data/operating-system/license_activation":       `"state"`,
	"/meta-data/operating-system/license_activation/state": ".metadata.instance.operating_system.license_activation.state",
	"/meta-data/operating-system/image_tag":                ".metadata.instance.operating_system.image_tag",
	"/meta-data/public-keys":                               ".metadata.instance.ssh_keys[]?",
	"/meta-data/spot":                                      `"termination-time"`,
	"/meta-data/spot/termination-time":                     ".metadata.instance.spot.termination_time",
	"/meta-data/public-ipv4":                               ".metadata.instance.network.addresses[]? | select(.address_family == 4 and .public == true) | .address",
	"/meta-data/public-ipv6":                               ".metadata.instance.network.addresses[]? | select(.address_family == 6 and .public == true) | .address",
	"/meta-data/local-ipv4":                                ".metadata.instance.network.addresses[]? | select(.address_family == 4 and .public == false) | .address",
}

// GetMetadataHandler provides an http handler that retrieves metadata using client filtering it
// using filter. filter should be a jq compatible processing string. Data is only filtered when
// using the TinkServer data model.
func getMetadataHandler(logger log.Logger, client hardware.Client, filter string, model datamodel.DataModel) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		logger.Debug("retrieving metadata")
		userIP := getIPFromRequest(r)
		if userIP == "" {
			return
		}

		metrics.MetadataRequests.Inc()
		l := logger.With("userIP", userIP)
		l.Info("got ip from request")
		hw, err := client.ByIP(r.Context(), userIP)
		if err != nil {
			metrics.Errors.WithLabelValues("metadata", "lookup").Inc()
			l.With("error", err).Info("failed to get hardware by ip")
			w.WriteHeader(http.StatusNotFound)
			return
		}

		hardware, err := hw.Export()
		if err != nil {
			l.With("error", err).Info("failed to export hardware")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if model == datamodel.TinkServer || model == datamodel.Kubernetes {
			hardware, err = filterMetadata(hardware, filter)
			if err != nil {
				l.With("error", err).Info("failed to filter metadata")
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}

		w.WriteHeader(http.StatusOK)
		_, err = w.Write(hardware)
		if err != nil {
			l.With("error", err).Info("failed to write response")
		}
	})
}

func ec2MetadataHandler(logger log.Logger, client hardware.Client) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		logger.Debug("calling EC2MetadataHandler")
		userIP := getIPFromRequest(r)
		if userIP == "" {
			logger.Info("Could not retrieve IP address")
			return
		}

		metrics.MetadataRequests.Inc()
		logger := logger.With("userIP", userIP)
		logger.Info("Retrieved IP peer IP")

		hw, err := client.ByIP(r.Context(), userIP)
		if err != nil {
			metrics.Errors.WithLabelValues("metadata", "lookup").Inc()
			logger.With("error", err).Info("failed to get hardware by ip")
			w.WriteHeader(http.StatusNotFound)
			return
		}

		ehw, err := hw.Export()
		if err != nil {
			logger.With("error", err).Info("failed to export hardware")
			w.WriteHeader(http.StatusInternalServerError)
			_, err := w.Write([]byte("404 not found"))
			if err != nil {
				logger.With("error", err).Info("failed to write response")
			}
			return
		}

		logger.With("exported", string(ehw)).Debug("Exported hardware")

		filter, err := processEC2Query(r.URL.Path)
		if err != nil {
			logger.With("error", err).Info("failed to process ec2 query")
			w.WriteHeader(http.StatusNotFound)
			_, err := w.Write([]byte("404 not found"))
			if err != nil {
				logger.With("error", err).Info("failed to write response")
			}
			return
		}

		resp, err := filterMetadata(ehw, filter)
		if err != nil {
			logger.With("error", err).Info("failed to filter metadata")
		}

		_, err = w.Write(resp)
		if err != nil {
			logger.With("error", err).Info("failed to write response")
		}
	})
}

func filterMetadata(hw []byte, filter string) ([]byte, error) {
	var result bytes.Buffer
	query, err := gojq.Parse(filter)
	if err != nil {
		return nil, err
	}
	input := make(map[string]interface{})
	err = json.Unmarshal(hw, &input)
	if err != nil {
		return nil, err
	}
	iter := query.Run(input)
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}

		if v == nil {
			continue
		}

		switch vv := v.(type) {
		case error:
			return nil, errors.Wrap(vv, "error while filtering with gojq")
		case string:
			result.WriteString(vv)
		default:
			marshalled, err := json.Marshal(vv)
			if err != nil {
				return nil, errors.Wrap(err, "error marshalling jq result")
			}
			result.Write(marshalled)
		}
		result.WriteRune('\n')
	}

	return bytes.TrimSuffix(result.Bytes(), []byte("\n")), nil
}

// processEC2Query returns either a specific filter (used to parse hardware data for the value of a specific field),
// or a comma-separated list of metadata items (to be printed).
func processEC2Query(url string) (string, error) {
	query := strings.TrimRight(strings.TrimPrefix(url, "/2009-04-04"), "/") // remove base pattern and trailing slash

	filter, ok := ec2Filters[query]
	if !ok {
		return "", errors.Errorf("invalid metadata item: %v", query)
	}

	return filter, nil
}

func getIPFromRequest(r *http.Request) string {
	addr := r.RemoteAddr
	if strings.ContainsRune(addr, ':') {
		addr, _, _ = net.SplitHostPort(addr)
	}
	return addr
}
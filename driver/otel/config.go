//
// Copyright (C) 2024 IBM Corporation.
//
// Authors:
// Frederico Araujo <frederico.araujo@ibm.com>
// Teryl Taylor <terylt@ibm.com>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package otel implements pluggable drivers for otel ingestion.
package otel

import (
	"fmt"

	"github.com/confluentinc/confluent-kafka-go/v2/kafka"
)

// Configuration keys.
const (
	KafkaConfigKey   string = "kafka.config"
	KafkaTopicsKey   string = "kafka.topics"
	KafkaEncodingKey string = "kafka.encoding"
)

// KafkaConfig holds Kafka output specific configuration.
type KafkaConfig struct {
	ConfigMap kafka.ConfigMap
	Topics    []string
	Encoding  Encoding
}

// CreateKafkaConfig creates a new config object from config dictionary.
func CreateKafkaConfig(conf map[string]interface{}) (c KafkaConfig, err error) {
	// default values
	c = KafkaConfig{ConfigMap: kafka.ConfigMap{
		"group.id":          "sfprocessor-otel-kafka-driver",
		"auto.offset.reset": "earliest",
	}, Topics: nil, Encoding: ProtoEncoding}

	// parse config map
	if v, ok := conf[KafkaConfigKey].(map[string]interface{}); ok {
		for key, value := range v {
			c.ConfigMap.SetKey(key, value)
		}
		if _, ok := c.ConfigMap["bootstrap.servers"]; !ok {
			return c, fmt.Errorf("no broker list found to initialize the kafka consumer")
		}
	} else {
		return c, fmt.Errorf("no kafka config map defined in configuration")
	}
	if v, ok := conf[KafkaTopicsKey].([]interface{}); ok {
		topics := []string{}
		for _, value := range v {
			topics = append(topics, value.(string))
		}
		c.Topics = topics
	} else {
		return c, fmt.Errorf("no kafka topics defined in configuration")
	}
	if v, ok := conf[KafkaEncodingKey].(string); ok {
		c.Encoding = parseEncodingConfig(v)
	}
	return
}

// Encoding type.
type Encoding int

// Transport config options.
const (
	ProtoEncoding Encoding = iota
	JSONEncoding
)

func (s Encoding) String() string {
	return [...]string{"proto", "json"}[s]
}

func parseEncodingConfig(s string) Encoding {
	if ProtoEncoding.String() == s {
		return ProtoEncoding
	}
	if JSONEncoding.String() == s {
		return JSONEncoding
	}
	return ProtoEncoding
}

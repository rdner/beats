// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package containerv2

import (
	"github.com/elastic/beats/v7/filebeat/beater"
	"github.com/elastic/beats/v7/filebeat/input/filestream"
	input "github.com/elastic/beats/v7/filebeat/input/v2"
	"github.com/elastic/beats/v7/libbeat/feature"
	conf "github.com/elastic/elastic-agent-libs/config"
	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/pkg/errors"
)

const pluginName = "containerv2"

// Plugin creates a new filestream input plugin for creating a stateful input.
func Plugin(log *logp.Logger, store beater.StateStore) input.Plugin {
	p := filestream.Plugin(log, store)

	p.Name = pluginName
	p.Stability = feature.Stable
	p.Deprecated = false
	p.Info = "container input"
	p.Doc = "The container input collects and parses the container logs"

	p.Manager = containerManagerWrapper{
		InputManager: p.Manager,
	}
	return p
}

type containerManagerWrapper struct {
	input.InputManager
}

func (m containerManagerWrapper) Create(cfg *conf.C) (input.Input, error) {
	config := defaultConfig()
	if err := cfg.Unpack(&config); err != nil {
		return nil, errors.Wrap(err, "reading container input config")
	}

	// set all container stuff here
	return m.InputManager.Create(cfg)
}

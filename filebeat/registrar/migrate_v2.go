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

package registrar

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/mapstr"
)

const (
	v2TmpSuffix = ".v2"
)

var (
	errCheckpointNotFound = errors.New("there is no checkpoint in the registry")
	checkpointFileRegexp  = regexp.MustCompile("^[0-9]+\\.json$")
)

type finalizer func() error

type v2Migrator struct {
	permissions os.FileMode
	regHome     string
}

func (m *v2Migrator) migrate() error {
	logp.Info("Migrate registry version 1 to version 2")

	logFinalizer, err := m.migrateCheckpoint()
	if err != nil {
		return fmt.Errorf("failed to migrate the checkpoint: %w", err)
	}

	checkpointFinalizer, err := m.migrateLog()
	if err != nil {
		return fmt.Errorf("failed to migrate the registry log file: %w", err)
	}

	err = logFinalizer()
	if err != nil {
		return fmt.Errorf("failed to finalize the registry log migration: %w", err)
	}
	err = checkpointFinalizer()
	if err != nil {
		return fmt.Errorf("failed to finalize the checkpoint migration: %w", err)
	}

	// update the log version, so we marked that the registry is migrated
	if err := ioutil.WriteFile(filepath.Join(m.regHome, "meta.json"), []byte(`{"version": "2"}`), m.permissions); err != nil {
		return fmt.Errorf("failed to update the meta.json file: %w", err)
	}

	return nil
}

func (m *v2Migrator) migrateCheckpoint() (finalizer, error) {
	origCheckpointFilename, err := m.findCheckpoint()

	switch {

	// there was no checkpoint which is normal, it's created only
	// if the registry log exceeds a size threshold
	case errors.Is(err, errCheckpointNotFound):
		return func() error { return nil }, nil

	case err != nil:
		return nil, fmt.Errorf("failed to look up a checkpoint: %w", err)

	default:
		origCheckpointFile, err := os.Open(origCheckpointFilename)
		if err != nil {
			return nil, fmt.Errorf("failed to open the checkpoint file %s: %w", origCheckpointFilename, err)
		}
		defer origCheckpointFile.Close()

		newCheckpointFilename := origCheckpointFilename + v2TmpSuffix
		err = m.createCheckpointVersion2(origCheckpointFile, newCheckpointFilename)
		if err != nil {
			return nil, fmt.Errorf("failed to create the converted checkpoint %s: %w", newCheckpointFilename, err)
		}

		finalizer := func() error {
			return m.replaceFile(origCheckpointFilename, newCheckpointFilename)
		}
		return finalizer, nil
	}
}

func (m *v2Migrator) findCheckpoint() (string, error) {
	entries, err := os.ReadDir(m.regHome)
	if err != nil {
		return "", fmt.Errorf("failed to read the directory %s: %w", m.regHome, err)
	}
	var checkpointEntries []os.FileInfo
	for _, entry := range entries {
		if !checkpointFileRegexp.MatchString(entry.Name()) {
			continue
		}
		infoEntry, err := entry.Info()
		if err != nil {
			return "", fmt.Errorf("failed to read the checkpoint file %s info: %w", entry.Name(), err)
		}
		logp.Info("Found checkpoint %s", filepath.Join(m.regHome, entry.Name()))
		checkpointEntries = append(checkpointEntries, infoEntry)
	}

	if len(checkpointEntries) == 0 {
		return "", errCheckpointNotFound
	}

	// the latest checkpoint should be on the top
	sort.Slice(checkpointEntries, func(i, j int) bool {
		return checkpointEntries[i].ModTime().Unix() > checkpointEntries[j].ModTime().Unix()
	})

	checkpoint := checkpointEntries[0]
	return filepath.Join(m.regHome, checkpoint.Name()), nil
}

func (m *v2Migrator) migrateLog() (finalizer, error) {
	origLogFilename := filepath.Join(m.regHome, "log.json")
	if !isFile(origLogFilename) {
		return nil, fmt.Errorf("missing original log file at %s", origLogFilename)
	}

	origLogFile, err := os.Open(origLogFilename)
	if err != nil {
		return nil, fmt.Errorf("failed to open original log file %s: %w", origLogFilename, err)
	}
	defer origLogFile.Close()

	logp.Info("Found registry log %s", origLogFilename)

	// creating a new converted log
	newLogFilename := origLogFilename + v2TmpSuffix
	err = m.createLogVersion2(origLogFile, newLogFilename)
	if err != nil {
		return nil, fmt.Errorf("failed to create a new registry log file %s: %w", newLogFilename, err)
	}

	finalizer := func() error {
		return m.replaceFile(origLogFilename, newLogFilename)
	}

	return finalizer, nil
}

func (m *v2Migrator) createCheckpointVersion2(srcCp io.Reader, dstFilename string) error {
	newCpFile, err := os.OpenFile(dstFilename, os.O_CREATE|os.O_TRUNC|os.O_APPEND|os.O_WRONLY, m.permissions)
	if err != nil {
		return fmt.Errorf("failed to create a new checkpoint file %s: %w", dstFilename, err)
	}
	defer newCpFile.Close()

	data, err := ioutil.ReadAll(srcCp)
	if err != nil {
		return fmt.Errorf("failed to read the checkpoint file: %w", err)
	}

	var states []mapstr.M
	err = json.Unmarshal(data, &states)
	if err != nil {
		return fmt.Errorf("failed to parse the states in the checkpoint: %w", err)
	}
	newStates := make([]mapstr.M, 0, len(states))

	for _, state := range states {
		key, err := state.GetValue("_key")
		if err != nil {
			newStates = append(newStates, state) // keep the state as it is
			continue
		}

		keyString, ok := key.(string)

		// replace the state with a converted state
		if ok && strings.HasPrefix(keyString, fileStatePrefix) {
			state = loginputToFilestream(state)
			_, err = state.Put("_key", loginputToFilestreamKey(keyString))
		}

		newStates = append(newStates, state)
	}

	stateBytes, err := json.Marshal(newStates)
	if err != nil {
		return fmt.Errorf("failed to serialize new states: %w", err)
	}

	_, err = newCpFile.Write(stateBytes)
	if err != nil {
		return fmt.Errorf("failed to write the states in the new checkpoint: %w", err)
	}

	return nil
}

func (m *v2Migrator) createLogVersion2(srcLog io.Reader, dstFilename string) error {
	reader := bufio.NewReader(srcLog)

	newLogFile, err := os.OpenFile(dstFilename, os.O_CREATE|os.O_TRUNC|os.O_APPEND|os.O_WRONLY, m.permissions)
	if err != nil {
		return fmt.Errorf("failed to create a new log file %s: %w", dstFilename, err)
	}
	defer newLogFile.Close()

	// reading the original log line by line and looking for state entries
	for {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read from the original log file: %w", err)
		}

		line, err = m.processLogEntry(line)
		if err != nil {
			return fmt.Errorf("failed to process log file entry: %w", err)
		}

		_, err = newLogFile.Write(line)
		if err != nil {
			return fmt.Errorf("failed to write a line to the new log file: %w", err)
		}
	}

	return nil
}

func (m *v2Migrator) processLogEntry(line []byte) ([]byte, error) {
	// we have to use the `mapstr.M` type because of the special JSON encoding/decoding
	var entry mapstr.M
	err := json.Unmarshal(line, &entry)
	if err != nil {
		return nil, fmt.Errorf("failed to parse a line from the original log file: %w", err)
	}

	// every state entry has a key
	key, err := entry.GetValue("k")
	keyString, keyExists := key.(string)

	if keyExists && strings.HasPrefix(keyString, fileStatePrefix) {
		_, err = entry.Put("k", loginputToFilestreamKey(keyString))
		if err != nil {
			return nil, fmt.Errorf("failed to replace a log entry key: %w", err)
		}

		value, err := entry.GetValue("v")
		if err != nil {
			return line, nil
		}

		valueMap, ok := value.(map[string]interface{})
		if !ok {
			return line, nil
		}

		newValue := loginputToFilestream(mapstr.M(valueMap))
		_, err = entry.Put("v", newValue)
		if err != nil {
			return nil, fmt.Errorf("failed to replace a log entry value: %w", err)
		}

		line, err = json.Marshal(entry)
		if err != nil {
			return nil, fmt.Errorf("failed to produce a replacement entry for the new log file: %w", err)
		}
		// `json.Marshal` does not add a trailing new line and it's required in the log
		line = append(line, '\n')
	}

	return line, nil
}

func (m *v2Migrator) replaceFile(orig, new string) error {
	// save the original file as a backup
	backupFilename := orig + ".bak"
	err := os.Rename(orig, backupFilename)
	if err != nil {
		return fmt.Errorf("failed to rename the original file %s->%s: %w", orig, backupFilename, err)
	}

	// move the new file in the place of the original file
	err = os.Rename(new, orig)
	if err != nil {
		return fmt.Errorf("failed to rename the new file %s->%s: %w", new, orig, err)
	}
	return nil
}

func loginputToFilestreamKey(key string) string {
	// TODO replace with a constant
	return strings.ReplaceAll(key, fileStatePrefix, "filestream::__loginput-wrapper::")
}

// conversion from the log input type to the filestream input type
func loginputToFilestream(value mapstr.M) mapstr.M {
	newValue := make(mapstr.M)
	copyMapValue(value, newValue, "ttl", "ttl")
	copyMapValue(value, newValue, "timestamp", "updated")
	copyMapValue(value, newValue, "offset", "cursor.offset")
	copyMapValue(value, newValue, "source", "meta.source")
	copyMapValue(value, newValue, "identifier_name", "meta.identifier_name")
	return newValue
}

func copyMapValue(src, dst mapstr.M, srcKey, dstKey string) {
	value, err := src.GetValue(srcKey)
	if err != nil {
		return
	}
	_, err = dst.Put(dstKey, value)
	if err != nil {
		panic(err)
	}
}

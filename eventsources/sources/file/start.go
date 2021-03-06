/*
Copyright 2018 BlackRock, Inc.

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

package file

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/pkg/errors"
	"github.com/radovskyb/watcher"
	"github.com/sirupsen/logrus"

	"github.com/argoproj/argo-events/common/logging"
	"github.com/argoproj/argo-events/eventsources/common/fsevent"
	"github.com/argoproj/argo-events/eventsources/sources"
	apicommon "github.com/argoproj/argo-events/pkg/apis/common"
	"github.com/argoproj/argo-events/pkg/apis/eventsource/v1alpha1"
)

// EventListener implements Eventing for file event source
type EventListener struct {
	EventSourceName string
	EventName       string
	FileEventSource v1alpha1.FileEventSource
}

// GetEventSourceName returns name of event source
func (el *EventListener) GetEventSourceName() string {
	return el.EventSourceName
}

// GetEventName returns name of event
func (el *EventListener) GetEventName() string {
	return el.EventName
}

// GetEventSourceType return type of event server
func (el *EventListener) GetEventSourceType() apicommon.EventSourceType {
	return apicommon.FileEvent
}

// StartListening starts listening events
func (el *EventListener) StartListening(ctx context.Context, dispatch func([]byte) error) error {
	log := logging.FromContext(ctx).WithFields(map[string]interface{}{
		logging.LabelEventSourceType: el.GetEventSourceType(),
		logging.LabelEventSourceName: el.GetEventSourceName(),
		logging.LabelEventName:       el.GetEventName(),
	})
	defer sources.Recover(el.GetEventName())

	fileEventSource := &el.FileEventSource
	if fileEventSource.Polling {
		if err := el.listenEventsPolling(ctx, dispatch, log); err != nil {
			log.WithError(err).Errorln("failed to listen to events")
			return err
		}
	} else {
		if err := el.listenEvents(ctx, dispatch, log); err != nil {
			log.WithError(err).Errorln("failed to listen to events")
			return err
		}
	}
	return nil
}

// listenEvents listen to file related events.
func (el *EventListener) listenEvents(ctx context.Context, dispatch func([]byte) error, log *logrus.Entry) error {
	fileEventSource := &el.FileEventSource

	// create new fs watcher
	log.Infoln("setting up a new file watcher...")
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return errors.Wrapf(err, "failed to set up a file watcher for %s", el.GetEventName())
	}
	defer watcher.Close()

	// file descriptor to watch must be available in file system. You can't watch an fs descriptor that is not present.
	log.Infoln("adding directory to monitor for the watcher...")
	err = watcher.Add(fileEventSource.WatchPathConfig.Directory)
	if err != nil {
		return errors.Wrapf(err, "failed to add directory %s to the watcher for %s", fileEventSource.WatchPathConfig.Directory, el.GetEventName())
	}

	var pathRegexp *regexp.Regexp
	if fileEventSource.WatchPathConfig.PathRegexp != "" {
		log.WithField("regex", fileEventSource.WatchPathConfig.PathRegexp).Infoln("matching file path with configured regex...")
		pathRegexp, err = regexp.Compile(fileEventSource.WatchPathConfig.PathRegexp)
		if err != nil {
			return errors.Wrapf(err, "failed to match file path with configured regex %s for %s", fileEventSource.WatchPathConfig.PathRegexp, el.GetEventName())
		}
	}

	log.Info("listening to file notifications...")
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				log.Info("fs watcher has stopped")
				// watcher stopped watching file events
				return errors.Errorf("fs watcher stopped for %s", el.GetEventName())
			}
			// fwc.Path == event.Name is required because we don't want to send event when .swp files are created
			matched := false
			relPath := strings.TrimPrefix(event.Name, fileEventSource.WatchPathConfig.Directory)
			if fileEventSource.WatchPathConfig.Path != "" && fileEventSource.WatchPathConfig.Path == relPath {
				matched = true
			} else if pathRegexp != nil && pathRegexp.MatchString(relPath) {
				matched = true
			}
			if matched && fileEventSource.EventType == event.Op.String() {
				log.WithFields(
					map[string]interface{}{
						"event-type":      event.Op.String(),
						"descriptor-name": event.Name,
					},
				).Infoln("file event")

				// Assume fsnotify event has the same Op spec of our file event
				fileEvent := fsevent.Event{Name: event.Name, Op: fsevent.NewOp(event.Op.String())}
				payload, err := json.Marshal(fileEvent)
				if err != nil {
					log.WithError(err).Errorln("failed to marshal the event to the fs event")
					continue
				}
				log.WithFields(
					map[string]interface{}{
						"event-type":      event.Op.String(),
						"descriptor-name": event.Name,
					},
				).Infoln("dispatching file event on data channel...")
				err = dispatch(payload)
				if err != nil {
					log.WithError(err).Errorln("failed to dispatch event")
				}
			}
		case err := <-watcher.Errors:
			return errors.Wrapf(err, "failed to process %s", el.GetEventName())
		case <-ctx.Done():
			log.Infoln("event source has been stopped")
			return nil
		}
	}
}

// listenEvents listen to file related events using polling.
func (el *EventListener) listenEventsPolling(ctx context.Context, dispatch func([]byte) error, log *logrus.Entry) error {
	fileEventSource := &el.FileEventSource

	// create new fs watcher
	log.Infoln("setting up a new file polling watcher...")
	watcher := watcher.New()
	defer watcher.Close()

	// file descriptor to watch must be available in file system. You can't watch an fs descriptor that is not present.
	log.Infoln("adding directory to monitor for the watcher...")
	err := watcher.Add(fileEventSource.WatchPathConfig.Directory)
	if err != nil {
		return errors.Wrapf(err, "failed to add directory %s to the watcher for %s", fileEventSource.WatchPathConfig.Directory, el.GetEventName())
	}

	var pathRegexp *regexp.Regexp
	if fileEventSource.WatchPathConfig.PathRegexp != "" {
		log.WithField("regex", fileEventSource.WatchPathConfig.PathRegexp).Infoln("matching file path with configured regex...")
		pathRegexp, err = regexp.Compile(fileEventSource.WatchPathConfig.PathRegexp)
		if err != nil {
			return errors.Wrapf(err, "failed to match file path with configured regex %s for %s", fileEventSource.WatchPathConfig.PathRegexp, el.GetEventName())
		}
	}

	go func() {
		log.Info("listening to file notifications...")
		for {
			select {
			case event, ok := <-watcher.Event:
				if !ok {
					log.Info("fs watcher has stopped")
					// watcher stopped watching file events
					log.Errorf("fs watcher stopped for %s", el.GetEventName())
					return
				}
				// fwc.Path == event.Name is required because we don't want to send event when .swp files are created
				matched := false
				relPath := strings.TrimPrefix(event.Name(), fileEventSource.WatchPathConfig.Directory)
				if fileEventSource.WatchPathConfig.Path != "" && fileEventSource.WatchPathConfig.Path == relPath {
					matched = true
				} else if pathRegexp != nil && pathRegexp.MatchString(relPath) {
					matched = true
				}
				if matched && fileEventSource.EventType == event.Op.String() {
					log.WithFields(
						map[string]interface{}{
							"event-type":      event.Op.String(),
							"descriptor-name": event.Name(),
						},
					).Infoln("file event")

					// Assume fsnotify event has the same Op spec of our file event
					fileEvent := fsevent.Event{Name: event.Name(), Op: fsevent.NewOp(event.Op.String())}
					payload, err := json.Marshal(fileEvent)
					if err != nil {
						log.WithError(err).Errorln("failed to marshal the event to the fs event")
						continue
					}
					log.WithFields(
						map[string]interface{}{
							"event-type":      event.Op.String(),
							"descriptor-name": event.Name(),
						},
					).Infoln("dispatching file event on data channel...")
					err = dispatch(payload)
					if err != nil {
						log.WithError(err).Errorln("failed to dispatch event")
					}
				}
			case err := <-watcher.Error:
				log.WithError(err).Errorf("failed to process %s", el.GetEventName())
				return
			case <-ctx.Done():
				log.Infoln("event source has been stopped")
				return
			}
		}
	}()
	log.Info("Starting watcher...")
	if err = watcher.Start(time.Millisecond * 100); err != nil {
		return errors.Wrapf(err, "Failed to start watcher for %s", el.GetEventName())
	}
	return nil
}

/*
   Copyright 2020 Docker Compose CLI authors

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

package compose

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/docker/docker/errdefs"

	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/progress"
	"github.com/docker/compose/v2/pkg/utils"

	"github.com/compose-spec/compose-go/types"
	moby "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

func (s *composeService) Start(ctx context.Context, projectName string, options api.StartOptions) error {
	return progress.Run(ctx, func(ctx context.Context) error {
		return s.start(ctx, strings.ToLower(projectName), options, nil)
	}, s.stdinfo())
}

func (s *composeService) start(ctx context.Context, projectName string, options api.StartOptions, listener api.ContainerEventListener) error {
	project := options.Project
	if project == nil {
		var containers Containers
		containers, err := s.getContainers(ctx, projectName, oneOffExclude, true)
		if err != nil {
			return err
		}

		project, err = s.projectFromName(containers, projectName, options.AttachTo...)
		if err != nil {
			return err
		}
	}

	eg, ctx := errgroup.WithContext(ctx)
	if listener != nil {
		attached, err := s.attach(ctx, project, listener, options.AttachTo)
		if err != nil {
			return err
		}

		eg.Go(func() error {
			return s.watchContainers(context.Background(), project.Name, options.AttachTo, options.Services, listener, attached,
				func(container moby.Container, _ time.Time) error {
					return s.attachContainer(ctx, container, listener)
				}, func(container moby.Container, _ time.Time) error {
					listener(api.ContainerEvent{
						Type:      api.ContainerEventAttach,
						Container: "", // actual name will be set by start event
						ID:        container.ID,
						Service:   container.Labels[api.ServiceLabel],
					})
					return nil
				})
		})
	}

	var containers Containers
	containers, err := s.apiClient().ContainerList(ctx, moby.ContainerListOptions{
		Filters: filters.NewArgs(
			projectFilter(project.Name),
			oneOffFilter(false),
		),
		All: true,
	})
	if err != nil {
		return err
	}

	err = InDependencyOrder(ctx, project, func(c context.Context, name string) error {
		service, err := project.GetService(name)
		if err != nil {
			return err
		}

		return s.startService(ctx, project, service, containers)
	})
	if err != nil {
		return err
	}

	if options.Wait {
		depends := types.DependsOnConfig{}
		for _, s := range project.Services {
			depends[s.Name] = types.ServiceDependency{
				Condition: getDependencyCondition(s, project),
			}
		}
		if options.WaitTimeout > 0 {
			withTimeout, cancel := context.WithTimeout(ctx, options.WaitTimeout)
			ctx = withTimeout
			defer cancel()
		}

		err = s.waitDependencies(ctx, project, depends, containers)
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("application not healthy after %s", options.WaitTimeout)
			}
			return err
		}
	}

	return eg.Wait()
}

// getDependencyCondition checks if service is depended on by other services
// with service_completed_successfully condition, and applies that condition
// instead, or --wait will never finish waiting for one-shot containers
func getDependencyCondition(service types.ServiceConfig, project *types.Project) string {
	for _, services := range project.Services {
		for dependencyService, dependencyConfig := range services.DependsOn {
			if dependencyService == service.Name && dependencyConfig.Condition == types.ServiceConditionCompletedSuccessfully {
				return types.ServiceConditionCompletedSuccessfully
			}
		}
	}
	return ServiceConditionRunningOrHealthy
}

type containerWatchFn func(container moby.Container, t time.Time) error

// watchContainers uses engine events to capture container start/die and notify ContainerEventListener
func (s *composeService) watchContainers(ctx context.Context, //nolint:gocyclo
	projectName string, services, required []string,
	listener api.ContainerEventListener, containers Containers, onStart, onRecreate containerWatchFn) error {
	if len(containers) == 0 {
		return nil
	}
	if len(required) == 0 {
		required = services
	}

	var (
		expected []string
		watched  = map[string]int{}
		replaced []string
	)
	for _, c := range containers {
		if utils.Contains(required, c.Labels[api.ServiceLabel]) {
			expected = append(expected, c.ID)
		}
		watched[c.ID] = 0
	}

	ctx, stop := context.WithCancel(ctx)
	err := s.Events(ctx, projectName, api.EventsOptions{
		Services: services,
		Consumer: func(event api.Event) error {
			inspected, err := s.apiClient().ContainerInspect(ctx, event.Container)
			if err != nil {
				if errdefs.IsNotFound(err) {
					// it's possible to get "destroy" or "kill" events but not
					// be able to inspect in time before they're gone from the
					// API, so just remove the watch without erroring
					delete(watched, event.Container)
					expected = utils.Remove(expected, event.Container)
					return nil
				}
				return err
			}
			container := moby.Container{
				ID:     inspected.ID,
				Names:  []string{inspected.Name},
				Labels: inspected.Config.Labels,
			}
			name := getContainerNameWithoutProject(container)

			service := container.Labels[api.ServiceLabel]
			switch event.Status {
			case "stop":
				if _, ok := watched[container.ID]; ok {
					eType := api.ContainerEventStopped
					if utils.Contains(replaced, container.ID) {
						utils.Remove(replaced, container.ID)
						eType = api.ContainerEventRecreated
					}
					listener(api.ContainerEvent{
						Type:      eType,
						Container: name,
						ID:        container.ID,
						Service:   service,
					})
				}

				delete(watched, container.ID)
				expected = utils.Remove(expected, container.ID)
			case "die":
				restarted := watched[container.ID]
				watched[container.ID] = restarted + 1
				// Container terminated.
				willRestart := inspected.State.Restarting

				eType := api.ContainerEventExit
				if utils.Contains(replaced, container.ID) {
					utils.Remove(replaced, container.ID)
					eType = api.ContainerEventRecreated
				}

				listener(api.ContainerEvent{
					Type:       eType,
					Container:  name,
					ID:         container.ID,
					Service:    service,
					ExitCode:   inspected.State.ExitCode,
					Restarting: willRestart,
				})

				if !willRestart {
					// we're done with this one
					delete(watched, container.ID)
					expected = utils.Remove(expected, container.ID)
				}
			case "start":
				count, ok := watched[container.ID]
				mustAttach := ok && count > 0 // Container restarted, need to re-attach
				if !ok {
					// A new container has just been added to service by scale
					watched[container.ID] = 0
					expected = append(expected, container.ID)
					mustAttach = true
				}
				if mustAttach {
					// Container restarted, need to re-attach
					err := onStart(container, event.Timestamp)
					if err != nil {
						return err
					}
				}
			case "create":
				if id, ok := container.Labels[api.ContainerReplaceLabel]; ok {
					replaced = append(replaced, id)
					err = onRecreate(container, event.Timestamp)
					if err != nil {
						return err
					}
					if utils.StringContains(expected, id) {
						expected = append(expected, inspected.ID)
					}
					watched[container.ID] = 1
					if utils.Contains(expected, id) {
						expected = append(expected, container.ID)
					}
				}
			}
			if len(expected) == 0 {
				stop()
			}
			return nil
		},
	})
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil
	}
	return err
}

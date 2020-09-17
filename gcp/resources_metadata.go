package gcp

import (
	"encoding/base32"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/common/log"
	"golang.org/x/net/context"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	crmv2 "google.golang.org/api/cloudresourcemanager/v2"
)

type resourceMetadata struct {
	id          string
	displayName string
	owner       string
	costCentre  string
	parent      string
}

type resourcesMetadata struct {
	metadataByProjectID map[string]*resourceMetadata
	metadataByID        map[string]*resourceMetadata
	ownerLabel          string
	costCentreLabel     string
	lastUpdate          time.Time
	updateLock          sync.Mutex
	clock               Clock
}

func newResourcesMetadata() *resourcesMetadata {
	return &resourcesMetadata{
		metadataByProjectID: make(map[string]*resourceMetadata),
		metadataByID:        make(map[string]*resourceMetadata),
		clock:               &realClock{},
	}
}

func (r *resourcesMetadata) WithResourceLabels(owner string, costCentre string) *resourcesMetadata {
	r.ownerLabel = owner
	r.costCentreLabel = costCentre
	return r
}

func (r *resourcesMetadata) ingest(e *resourceMetadata) {
	if strings.HasPrefix(e.id, "projects/") {
		r.metadataByProjectID[e.displayName] = e
	} else {
		r.metadataByID[e.id] = e
	}
}

func (r *resourcesMetadata) path(e *resourceMetadata) []string {
	if e.parent != "" {
		if parent, ok := r.metadataByID[e.parent]; ok {
			return append(r.path(parent), parent.displayName)
		}
	}
	return []string{}
}

func (r *resourcesMetadata) projectByID(id string) *resourceMetadata {
	return r.metadataByProjectID[id]
}

func (r *resourcesMetadata) update(ctx context.Context) error {
	r.updateLock.Lock()
	defer r.updateLock.Unlock()

	// cache information for one hour
	if r.clock.Now().Before(r.lastUpdate.Add(time.Hour)) {
		return nil
	}

	log.Debug("renew resource metadata from GCP resourcemanager")

	crmv1Service, err := crmv1.NewService(ctx)
	if err != nil {
		return err
	}

	crmv2Service, err := crmv2.NewService(ctx)
	if err != nil {
		return err
	}

	// list projects
	{
		req := crmv1Service.Projects.List()
		if err := req.Pages(ctx, func(page *crmv1.ListProjectsResponse) error {
			for _, e := range page.Projects {
				var owner, costCentre string
				if value, ok := e.Labels[r.ownerLabel]; ok {
					value = strings.ToUpper(strings.ReplaceAll(value, "_", "="))
					if valueDecoded, err := base32.StdEncoding.DecodeString(value); err != nil {
						log.Warnf("error decoding label '%s=%s' of project '%s': %s", r.ownerLabel, value, e.ProjectId, err)
					} else {
						owner = string(valueDecoded)
					}
				}

				if value, ok := e.Labels[r.costCentreLabel]; ok {
					value = strings.ToUpper(strings.ReplaceAll(value, "_", "="))
					costCentre = strings.ToLower(value)
				}

				r.ingest(&resourceMetadata{
					id:          fmt.Sprintf("projects/%d", e.ProjectNumber),
					displayName: e.ProjectId,
					owner:       owner,
					costCentre:  costCentre,
					parent:      fmt.Sprintf("%ss/%s", e.Parent.Type, e.Parent.Id),
				})
			}
			return nil
		}); err != nil {
			return err
		}
	}

	// list folders
	{
		req := crmv2Service.Folders.Search(&crmv2.SearchFoldersRequest{})
		if err := req.Pages(ctx, func(page *crmv2.SearchFoldersResponse) error {
			for _, e := range page.Folders {
				r.ingest(&resourceMetadata{
					id:          e.Name,
					displayName: e.DisplayName,
					parent:      e.Parent,
				})
			}
			return nil
		}); err != nil {
			return err
		}
	}

	// list organizations
	{
		req := crmv1Service.Organizations.Search(&crmv1.SearchOrganizationsRequest{})
		if err := req.Pages(ctx, func(page *crmv1.SearchOrganizationsResponse) error {
			for _, e := range page.Organizations {
				r.ingest(&resourceMetadata{
					id:          e.Name,
					displayName: e.DisplayName,
				})
			}
			return nil
		}); err != nil {
			return err
		}
	}

	r.lastUpdate = r.clock.Now()

	return nil
}

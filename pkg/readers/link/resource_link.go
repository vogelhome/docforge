// SPDX-FileCopyrightText: 2023 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package link

import (
	"fmt"
	"net/url"
	"regexp"
)

// Resource represents a GitHub resource URL
type Resource struct {
	url.URL
	Host  string
	Owner string
	Repo  string
	Type  string
	Ref   string
	Path  string
}

var (
	rawPrefixed       = regexp.MustCompile(`https://([^/]+)/raw/([^/]+)/([^/]+)/([^/]+)/([^\?#]+).*`)
	absLink           = regexp.MustCompile(`https://([^/]+)/([^/]+)/([^/]+)/([^/]+)/([^/]+)/([^\?#]+).*`)
	githubusercontent = regexp.MustCompile(`https://raw.githubusercontent.com/([^/]+)/([^/]+)/([^/]+)/([^\?#]+).*`)
	relative          = regexp.MustCompile(`([^\?#]+).*`)
)

// NewResource creates new resource from url as string
func NewResource(URL string) (Resource, error) {
	u, err := url.Parse(URL)
	if err != nil {
		return Resource{}, err
	}
	return NewResourceFromURL(u)
}

// NewResourceFromURL creates new resource from url object
func NewResourceFromURL(u *url.URL) (Resource, error) {
	components := rawPrefixed.FindStringSubmatch(u.String())
	if components != nil {
		return Resource{
			URL:   *u,
			Host:  components[1],
			Owner: components[2],
			Repo:  components[3],
			Type:  "raw",
			Ref:   components[4],
			Path:  components[5],
		}, nil
	}
	components = githubusercontent.FindStringSubmatch(u.String())
	if components != nil {
		return Resource{
			URL:   *u,
			Host:  "github.com",
			Owner: components[1],
			Repo:  components[2],
			Type:  "raw",
			Ref:   components[3],
			Path:  components[4],
		}, nil
	}
	components = absLink.FindStringSubmatch(u.String())
	if components != nil {
		return Resource{
			URL:   *u,
			Host:  components[1],
			Owner: components[2],
			Repo:  components[3],
			Type:  components[4],
			Ref:   components[5],
			Path:  components[6],
		}, nil
	}
	components = relative.FindStringSubmatch(u.String())
	if components != nil {
		return Resource{
			URL:  *u,
			Path: components[1],
		}, nil
	}
	return Resource{}, nil
}

// GetResourceURL returns the u
func (r *Resource) GetResourceURL() string {
	return fmt.Sprintf("https://%s/%s/%s/%s/%s/%s", r.Host, r.Owner, r.Repo, r.Type, r.Ref, r.Path)
}

// GetRepoURL returns the GitHub repository URL
func (r *Resource) GetRepoURL() string {
	return fmt.Sprintf("https://%s/%s/%s", r.Host, r.Owner, r.Repo)
}

// GetRawURL returns the GitHub raw URL if the resource is 'blob', otherwise returns the origin URL
func (r *Resource) GetRawURL() string {
	return fmt.Sprintf("https://%s/%s/%s/raw/%s/%s", r.Host, r.Owner, r.Repo, r.Ref, r.Path)
}

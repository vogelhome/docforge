// SPDX-FileCopyrightText: 2023 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package repositoryhostsfakes

import "embed"

// FilesystemRegistry builds fake registry from directory
func FilesystemRegistry(dir embed.FS) *FakeRegistry {
	localHost := FakeRepositoryHost{}
	localHost.ManifestFromURLCalls(func(url string) (string, error) {
		content, err := dir.ReadFile(url)
		return string(content), err
	})
	localHost.ToAbsLinkCalls(func(url, link string) (string, error) {
		return link, nil
	})
	registry := &FakeRegistry{}
	registry.GetReturns(&localHost, nil)
	return registry
}

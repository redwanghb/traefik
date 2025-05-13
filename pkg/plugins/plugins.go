package plugins

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/go-multierror"
	"github.com/rs/zerolog/log"
)

const localGoPath = "./plugins-local/"

// SetupRemotePlugins setup remote plugins environment.
func SetupRemotePlugins(client *Client, plugins map[string]Descriptor) error {
	// 检查plugins配置是否有问题
	err := checkRemotePluginsConfiguration(plugins)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	// 检查state.json文件，清空archives文件夹
	// 这个函数会检查./plugins-storage/state.json文件是否存在
	// 检查./plugins-storage/archives文件夹中的插件版本是否与state.json中描述的一致，不一致就清空archives文件夹
	// 源码中这里未关闭文件句柄
	err = client.CleanArchives(plugins)
	if err != nil {
		return fmt.Errorf("unable to clean archives: %w", err)
	}

	ctx := context.Background()

	for pAlias, desc := range plugins {
		log.Ctx(ctx).Debug().Msgf("Loading of plugin: %s: %s@%s", pAlias, desc.ModuleName, desc.Version)

		// 从https://plugins.traefik.io/public/download下载插件，并返回插件的hash值，如果本地已经存在对应的插件，则不下载
		hash, err := client.Download(ctx, desc.ModuleName, desc.Version)
		if err != nil {
			_ = client.ResetAll()
			return fmt.Errorf("unable to download plugin %s: %w", desc.ModuleName, err)
		}

		// 在HTTP请求头中设置X-Plugin-Hash，检查下载的文件是否正确
		err = client.Check(ctx, desc.ModuleName, desc.Version, hash)
		if err != nil {
			_ = client.ResetAll()
			return fmt.Errorf("unable to check archive integrity of the plugin %s: %w", desc.ModuleName, err)
		}
	}

	// 将插件的描述信息写入到./plugins-storage/state.json文件中
	err = client.WriteState(plugins)
	if err != nil {
		_ = client.ResetAll()
		return fmt.Errorf("unable to write plugins state: %w", err)
	}

	// 解压下载的插件
	for _, desc := range plugins {
		err = client.Unzip(desc.ModuleName, desc.Version)
		if err != nil {
			_ = client.ResetAll()
			return fmt.Errorf("unable to unzip archive: %w", err)
		}
	}

	return nil
}

func checkRemotePluginsConfiguration(plugins map[string]Descriptor) error {
	if plugins == nil {
		return nil
	}

	uniq := make(map[string]struct{})

	var errs []string
	for pAlias, descriptor := range plugins {
		if descriptor.ModuleName == "" {
			errs = append(errs, fmt.Sprintf("%s: plugin name is missing", pAlias))
		}

		if descriptor.Version == "" {
			errs = append(errs, fmt.Sprintf("%s: plugin version is missing", pAlias))
		}

		if strings.HasPrefix(descriptor.ModuleName, "/") || strings.HasSuffix(descriptor.ModuleName, "/") {
			errs = append(errs, fmt.Sprintf("%s: plugin name should not start or end with a /", pAlias))
			continue
		}

		if _, ok := uniq[descriptor.ModuleName]; ok {
			errs = append(errs, fmt.Sprintf("only one version of a plugin is allowed, there is a duplicate of %s", descriptor.ModuleName))
			continue
		}

		uniq[descriptor.ModuleName] = struct{}{}
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, ": "))
	}

	return nil
}

// SetupLocalPlugins setup local plugins environment.
func SetupLocalPlugins(plugins map[string]LocalDescriptor) error {
	if plugins == nil {
		return nil
	}

	uniq := make(map[string]struct{})

	var errs *multierror.Error
	for pAlias, descriptor := range plugins {
		//检查配置是否正确
		if descriptor.ModuleName == "" {
			errs = multierror.Append(errs, fmt.Errorf("%s: plugin name is missing", pAlias))
		}

		if strings.HasPrefix(descriptor.ModuleName, "/") || strings.HasSuffix(descriptor.ModuleName, "/") {
			errs = multierror.Append(errs, fmt.Errorf("%s: plugin name should not start or end with a /", pAlias))
			continue
		}

		if _, ok := uniq[descriptor.ModuleName]; ok {
			errs = multierror.Append(errs, fmt.Errorf("only one version of a plugin is allowed, there is a duplicate of %s", descriptor.ModuleName))
			continue
		}

		uniq[descriptor.ModuleName] = struct{}{}

		// 检查./plugins-local/src/下的插件的yml文件是否正确
		err := checkLocalPluginManifest(descriptor)
		errs = multierror.Append(errs, err)
	}

	return errs.ErrorOrNil()
}

func checkLocalPluginManifest(descriptor LocalDescriptor) error {
	// 从./plugins-local/src/下读取模块的yml文件到Manifest结构体中
	m, err := ReadManifest(localGoPath, descriptor.ModuleName)
	if err != nil {
		return err
	}

	var errs *multierror.Error

	// 根据Manifest中的Type成员判定是否是支持的runtime，支持的runtime有yaegi和wasm
	switch m.Type {
	case typeMiddleware:
		if m.Runtime != runtimeYaegi && m.Runtime != runtimeWasm && m.Runtime != "" {
			errs = multierror.Append(errs, fmt.Errorf("%s: unsupported runtime '%q'", descriptor.ModuleName, m.Runtime))
		}

	case typeProvider:
		if m.Runtime != runtimeYaegi && m.Runtime != "" {
			errs = multierror.Append(errs, fmt.Errorf("%s: unsupported runtime '%q'", descriptor.ModuleName, m.Runtime))
		}

	default:
		errs = multierror.Append(errs, fmt.Errorf("%s: unsupported type %q", descriptor.ModuleName, m.Type))
	}

	if m.IsYaegiPlugin() {
		if m.Import == "" {
			errs = multierror.Append(errs, fmt.Errorf("%s: missing import", descriptor.ModuleName))
		}

		if !strings.HasPrefix(m.Import, descriptor.ModuleName) {
			errs = multierror.Append(errs, fmt.Errorf("the import %q must be related to the module name %q", m.Import, descriptor.ModuleName))
		}
	}

	if m.DisplayName == "" {
		errs = multierror.Append(errs, fmt.Errorf("%s: missing DisplayName", descriptor.ModuleName))
	}

	if m.Summary == "" {
		errs = multierror.Append(errs, fmt.Errorf("%s: missing Summary", descriptor.ModuleName))
	}

	if m.TestData == nil {
		errs = multierror.Append(errs, fmt.Errorf("%s: missing TestData", descriptor.ModuleName))
	}

	return errs.ErrorOrNil()
}

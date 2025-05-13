package main

import (
	"fmt"

	"github.com/traefik/traefik/v3/pkg/config/static"
	"github.com/traefik/traefik/v3/pkg/plugins"
)

const outputDir = "./plugins-storage/"

func createPluginBuilder(staticConfiguration *static.Configuration) (*plugins.Builder, error) {
	// 初始化插件，具体参考函数内部说明
	client, plgs, localPlgs, err := initPlugins(staticConfiguration)
	if err != nil {
		return nil, err
	}

	// 创建插件构建器，里面包含了wasm或者yaegi的插件结构体，包含两种类型的插件，一种是middleware，一种是provider
	return plugins.NewBuilder(client, plgs, localPlgs)
}

func initPlugins(staticCfg *static.Configuration) (*plugins.Client, map[string]plugins.Descriptor, map[string]plugins.LocalDescriptor, error) {
	// 检查Plugins和LocalPlugins的名称是否重复，从代码中看绝对不允许重复
	err := checkUniquePluginNames(staticCfg.Experimental)
	if err != nil {
		return nil, nil, nil, err
	}

	// 定义客户端和全部插件描述信息变量
	var client *plugins.Client
	plgs := map[string]plugins.Descriptor{}

	// 如果静态配置中存在插件
	if hasPlugins(staticCfg) {
		// 设置插件的存储目录
		opts := plugins.ClientOptions{
			Output: outputDir,
		}

		// 创建了一个插件客户端，请求的是Traefik的地址https://plugins.traefik.io/public/
		// 在./plugins-storage/目录下创建了sources文件夹，并在其中创建了临时的文件夹gop-；同时创建了archives文件夹
		// 并将相关路径存储在plugins.Client结构体中
		var err error
		client, err = plugins.NewClient(opts)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("unable to create plugins client: %w", err)
		}

		// 根据配置下载插件
		// 检查下载的插件的MD5是否正确
		// 将插件的描述信息写入到./plugins-storage/state.json文件中
		// 解压下载的插件到sources文件夹中
		err = plugins.SetupRemotePlugins(client, staticCfg.Experimental.Plugins)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("unable to set up plugins environment: %w", err)
		}

		plgs = staticCfg.Experimental.Plugins
	}

	localPlgs := map[string]plugins.LocalDescriptor{}

	// 如果静态配置中存在本地插件
	if hasLocalPlugins(staticCfg) {
		err := plugins.SetupLocalPlugins(staticCfg.Experimental.LocalPlugins)
		if err != nil {
			return nil, nil, nil, err
		}

		localPlgs = staticCfg.Experimental.LocalPlugins
	}

	return client, plgs, localPlgs, nil
}

func checkUniquePluginNames(e *static.Experimental) error {
	if e == nil {
		return nil
	}

	for s := range e.LocalPlugins {
		if _, ok := e.Plugins[s]; ok {
			return fmt.Errorf("the plugin's name %q must be unique", s)
		}
	}

	return nil
}

func hasPlugins(staticCfg *static.Configuration) bool {
	return staticCfg.Experimental != nil && len(staticCfg.Experimental.Plugins) > 0
}

func hasLocalPlugins(staticCfg *static.Configuration) bool {
	return staticCfg.Experimental != nil && len(staticCfg.Experimental.LocalPlugins) > 0
}

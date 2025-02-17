package envoyextensions

import (
	"fmt"

	awslambda "github.com/hashicorp/consul/agent/envoyextensions/builtin/aws-lambda"
	"github.com/hashicorp/consul/agent/envoyextensions/builtin/http/localratelimit"
	"github.com/hashicorp/consul/agent/envoyextensions/builtin/lua"
	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/envoyextensions/extensioncommon"
	"github.com/hashicorp/go-multierror"
)

type extensionConstructor func(api.EnvoyExtension) (extensioncommon.EnvoyExtender, error)

var extensionConstructors = map[string]extensionConstructor{
	api.BuiltinLuaExtension:            lua.Constructor,
	api.BuiltinAWSLambdaExtension:      awslambda.Constructor,
	api.BuiltinLocalRatelimitExtension: localratelimit.Constructor,
}

// ConstructExtension attempts to lookup and build an extension from the registry with the
// given config. Returns an error if the extension does not exist, or if the extension fails
// to be constructed properly.
func ConstructExtension(ext api.EnvoyExtension) (extensioncommon.EnvoyExtender, error) {
	constructor, ok := extensionConstructors[ext.Name]
	if !ok {
		return nil, fmt.Errorf("name %q is not a built-in extension", ext.Name)
	}
	return constructor(ext)
}

// ValidateExtensions will attempt to construct each instance of the given envoy extension configurations
// and returns an error if any fail to build. Note that this step is separated from the xds package and
// does not check any potential runtime configuration that the extension could encounter -- it simply
// ensures that the extension can be built from the given arguments.
func ValidateExtensions(extensions []api.EnvoyExtension) error {
	var output error
	for i, ext := range extensions {
		if ext.Name == "" {
			output = multierror.Append(output, fmt.Errorf("invalid EnvoyExtensions[%d]: Name is required", i))
			continue
		}
		_, err := ConstructExtension(ext)
		if err != nil {
			output = multierror.Append(output, fmt.Errorf("invalid EnvoyExtensions[%d][%s]: %w", i, ext.Name, err))
			continue
		}
	}
	return output
}

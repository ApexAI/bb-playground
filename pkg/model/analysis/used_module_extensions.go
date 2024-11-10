package analysis

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/buildbarn/bb-playground/pkg/label"
	model_core "github.com/buildbarn/bb-playground/pkg/model/core"
	model_starlark "github.com/buildbarn/bb-playground/pkg/model/starlark"
	model_analysis_pb "github.com/buildbarn/bb-playground/pkg/proto/model/analysis"
	model_starlark_pb "github.com/buildbarn/bb-playground/pkg/proto/model/starlark"
	pg_starlark "github.com/buildbarn/bb-playground/pkg/starlark"
	"github.com/buildbarn/bb-playground/pkg/storage/dag"

	"go.starlark.net/starlark"
)

type usedModuleExtension struct {
	message model_analysis_pb.ModuleExtension
	users   map[label.ModuleInstance]*moduleExtensionUser
}

type moduleExtensionUser struct {
	message    model_analysis_pb.ModuleExtension_User
	tagClasses map[string]*model_analysis_pb.ModuleExtension_TagClass
}

type usedModuleExtensionOptions struct {
	environment UsedModuleExtensionsEnvironment
	patcher     *model_core.ReferenceMessagePatcher[dag.ObjectContentsWalker]
}

type usedModuleExtensionProxy struct {
	handler       *usedModuleExtensionExtractingModuleDotBazelHandler
	user          *moduleExtensionUser
	devDependency bool
}

func (p *usedModuleExtensionProxy) Tag(className string, attrs map[string]starlark.Value) error {
	meu := p.user
	tagClass, ok := meu.tagClasses[className]
	if !ok {
		tagClass = &model_analysis_pb.ModuleExtension_TagClass{
			Name: className,
		}
		meu.tagClasses[className] = tagClass
		meu.message.TagClasses = append(meu.message.TagClasses, tagClass)
	}

	encodedAttrs := make([]*model_starlark_pb.NamedValue, 0, len(attrs))
	for _, name := range slices.Sorted(maps.Keys(attrs)) {
		encodedValue, _, err := model_starlark.EncodeValue(
			attrs[name],
			map[starlark.Value]struct{}{},
			/* currentIdentifier = */ nil,
			p.handler.valueEncodingOptions,
		)
		if err != nil {
			return fmt.Errorf("tag class %s attr %s: %w", className, name)
		}
		encodedAttrs = append(encodedAttrs, &model_starlark_pb.NamedValue{
			Name:  name,
			Value: encodedValue.Message,
		})
		p.handler.options.patcher.Merge(encodedValue.Patcher)
	}

	tagClass.Tags = append(tagClass.Tags, &model_analysis_pb.ModuleExtension_Tag{
		IsDevDependency: p.devDependency,
		Attrs:           encodedAttrs,
	})
	return nil
}

func (usedModuleExtensionProxy) UseRepo(repos map[label.ApparentRepo]label.ApparentRepo) error {
	return nil
}

type usedModuleExtensionExtractingModuleDotBazelHandler struct {
	options               *usedModuleExtensionOptions
	moduleInstance        label.ModuleInstance
	isRoot                bool
	ignoreDevDependencies bool
	usedModuleExtensions  map[label.ModuleExtension]*usedModuleExtension
	valueEncodingOptions  *model_starlark.ValueEncodingOptions
}

func (usedModuleExtensionExtractingModuleDotBazelHandler) BazelDep(name label.Module, version *label.ModuleVersion, maxCompatibilityLevel int, repoName label.ApparentRepo, devDependency bool) error {
	return nil
}

func (usedModuleExtensionExtractingModuleDotBazelHandler) Module(name label.Module, version *label.ModuleVersion, compatibilityLevel int, repoName label.ApparentRepo, bazelCompatibility []string) error {
	return nil
}

func (usedModuleExtensionExtractingModuleDotBazelHandler) RegisterExecutionPlatforms(platformLabels []label.ApparentLabel, devDependency bool) error {
	return nil
}

func (usedModuleExtensionExtractingModuleDotBazelHandler) RegisterToolchains(toolchainLabels []label.ApparentLabel, devDependency bool) error {
	return nil
}

func (h *usedModuleExtensionExtractingModuleDotBazelHandler) UseExtension(extensionBzlFile label.ApparentLabel, extensionName label.StarlarkIdentifier, devDependency, isolate bool) (pg_starlark.ModuleExtensionProxy, error) {
	if devDependency && h.ignoreDevDependencies {
		return pg_starlark.NullModuleExtensionProxy, nil
	}

	// Look up the module extension properties, so that we obtain
	// the canonical identifier of the Starlark module_extension
	// declaration.
	canonicalExtensionBzlFile, err := resolveApparentLabel(h.options.environment, h.moduleInstance.GetBareCanonicalRepo(), extensionBzlFile)
	if err != nil {
		return nil, err
	}
	moduleExtensionIdentifier := canonicalExtensionBzlFile.AppendStarlarkIdentifier(extensionName)
	moduleExtensionName := moduleExtensionIdentifier.ToModuleExtension()
	moduleExtensionIdentifierStr := moduleExtensionIdentifier.String()

	ume, ok := h.usedModuleExtensions[moduleExtensionName]
	if ok {
		// Safety belt: prevent a single module instance from
		// declaring multiple module extensions with the same
		// name. This would cause collisions when both of them
		// declare repos with the same name.
		if moduleExtensionIdentifierStr != ume.message.Identifier {
			return nil, fmt.Errorf(
				"module extension declarations %#v and %#v have the same name, meaning they would both declare repos with prefix %#v",
				moduleExtensionIdentifierStr,
				ume.message.Identifier,
				moduleExtensionName.String(),
			)
		}
	} else {
		ume = &usedModuleExtension{
			message: model_analysis_pb.ModuleExtension{
				Identifier: moduleExtensionIdentifierStr,
			},
			users: map[label.ModuleInstance]*moduleExtensionUser{},
		}
		h.usedModuleExtensions[moduleExtensionName] = ume
	}

	meu, ok := ume.users[h.moduleInstance]
	if !ok {
		meu = &moduleExtensionUser{
			message: model_analysis_pb.ModuleExtension_User{
				ModuleInstance: h.moduleInstance.String(),
				IsRoot:         h.isRoot,
			},
			tagClasses: map[string]*model_analysis_pb.ModuleExtension_TagClass{},
		}
		ume.users[h.moduleInstance] = meu
		ume.message.Users = append(ume.message.Users, &meu.message)
	}

	return &usedModuleExtensionProxy{
		handler:       h,
		user:          meu,
		devDependency: devDependency,
	}, nil
}

func (usedModuleExtensionExtractingModuleDotBazelHandler) UseRepoRule(repoRuleBzlFile label.ApparentLabel, repoRuleName string) (pg_starlark.RepoRuleProxy, error) {
	return func(name label.ApparentRepo, devDependency bool, attrs map[string]starlark.Value) error {
		return nil
	}, nil
}

func (c *baseComputer) ComputeUsedModuleExtensionsValue(ctx context.Context, key *model_analysis_pb.UsedModuleExtensions_Key, e UsedModuleExtensionsEnvironment) (PatchedUsedModuleExtensionsValue, error) {
	options := usedModuleExtensionOptions{
		environment: e,
		patcher:     model_core.NewReferenceMessagePatcher[dag.ObjectContentsWalker](),
	}
	usedModuleExtensions := map[label.ModuleExtension]*usedModuleExtension{}
	isRoot := true
	if err := c.visitModuleDotBazelFilesBreadthFirst(ctx, e, func(moduleInstance label.ModuleInstance, ignoreDevDependencies bool) pg_starlark.ChildModuleDotBazelHandler {
		h := &usedModuleExtensionExtractingModuleDotBazelHandler{
			options:               &options,
			moduleInstance:        moduleInstance,
			isRoot:                isRoot,
			ignoreDevDependencies: ignoreDevDependencies,
			usedModuleExtensions:  usedModuleExtensions,
			valueEncodingOptions: c.getValueEncodingOptions(
				moduleInstance.GetBareCanonicalRepo().
					GetRootPackage().
					AppendTargetName(moduleDotBazelTargetName),
			),
		}
		isRoot = false
		return h
	}); err != nil {
		return PatchedUsedModuleExtensionsValue{}, err
	}

	sortedModuleExtensions := make([]*model_analysis_pb.ModuleExtension, 0, len(usedModuleExtensions))
	for _, name := range slices.SortedFunc(
		maps.Keys(usedModuleExtensions),
		func(a, b label.ModuleExtension) int { return strings.Compare(a.String(), b.String()) },
	) {
		sortedModuleExtensions = append(sortedModuleExtensions, &usedModuleExtensions[name].message)
	}
	return model_core.PatchedMessage[*model_analysis_pb.UsedModuleExtensions_Value, dag.ObjectContentsWalker]{
		Message: &model_analysis_pb.UsedModuleExtensions_Value{
			ModuleExtensions: sortedModuleExtensions,
		},
		Patcher: options.patcher,
	}, nil
}

package analysis

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/buildbarn/bb-playground/pkg/evaluation"
	"github.com/buildbarn/bb-playground/pkg/label"
	model_core "github.com/buildbarn/bb-playground/pkg/model/core"
	"github.com/buildbarn/bb-playground/pkg/model/core/inlinedtree"
	model_encoding "github.com/buildbarn/bb-playground/pkg/model/encoding"
	model_filesystem "github.com/buildbarn/bb-playground/pkg/model/filesystem"
	model_parser "github.com/buildbarn/bb-playground/pkg/model/parser"
	model_starlark "github.com/buildbarn/bb-playground/pkg/model/starlark"
	model_analysis_pb "github.com/buildbarn/bb-playground/pkg/proto/model/analysis"
	model_build_pb "github.com/buildbarn/bb-playground/pkg/proto/model/build"
	model_filesystem_pb "github.com/buildbarn/bb-playground/pkg/proto/model/filesystem"
	"github.com/buildbarn/bb-playground/pkg/storage/dag"
	"github.com/buildbarn/bb-playground/pkg/storage/object"
	re_filesystem "github.com/buildbarn/bb-remote-execution/pkg/filesystem"
	bb_path "github.com/buildbarn/bb-storage/pkg/filesystem/path"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.starlark.net/starlark"
)

type baseComputer struct {
	objectDownloader            object.Downloader[object.LocalReference]
	buildSpecificationReference object.LocalReference
	buildSpecificationEncoder   model_encoding.BinaryEncoder
	httpClient                  *http.Client
	filePool                    re_filesystem.FilePool
}

func NewBaseComputer(
	objectDownloader object.Downloader[object.LocalReference],
	buildSpecificationReference object.LocalReference,
	buildSpecificationEncoder model_encoding.BinaryEncoder,
	httpClient *http.Client,
	filePool re_filesystem.FilePool,
) Computer {
	return &baseComputer{
		objectDownloader:            objectDownloader,
		buildSpecificationReference: buildSpecificationReference,
		buildSpecificationEncoder:   buildSpecificationEncoder,
		httpClient:                  httpClient,
		filePool:                    filePool,
	}
}

func (c *baseComputer) getValueEncodingOptions(currentFilename label.CanonicalLabel) *model_starlark.ValueEncodingOptions {
	return &model_starlark.ValueEncodingOptions{
		CurrentFilename:        currentFilename,
		ObjectEncoder:          model_encoding.NewChainedBinaryEncoder(nil),
		ObjectReferenceFormat:  c.buildSpecificationReference.GetReferenceFormat(),
		ObjectMinimumSizeBytes: 32 * 1024,
		ObjectMaximumSizeBytes: 128 * 1024,
	}
}

func (c *baseComputer) getValueDecodingOptions(ctx context.Context, labelCreator func(label.CanonicalLabel) (starlark.Value, error)) *model_starlark.ValueDecodingOptions {
	return &model_starlark.ValueDecodingOptions{
		Context:          ctx,
		ObjectDownloader: c.objectDownloader,
		ObjectEncoder:    model_encoding.NewChainedBinaryEncoder(nil),
		LabelCreator:     labelCreator,
	}
}

func (c *baseComputer) getInlinedTreeOptions() *inlinedtree.Options {
	return &inlinedtree.Options{
		ReferenceFormat:  c.buildSpecificationReference.GetReferenceFormat(),
		Encoder:          model_encoding.NewChainedBinaryEncoder(nil),
		MaximumSizeBytes: 32 * 1024,
	}
}

type resolveApparentLabelEnvironment interface {
	GetCanonicalRepoNameValue(*model_analysis_pb.CanonicalRepoName_Key) model_core.Message[*model_analysis_pb.CanonicalRepoName_Value]
	GetRootModuleValue(*model_analysis_pb.RootModule_Key) model_core.Message[*model_analysis_pb.RootModule_Value]
}

func resolveApparentLabel(e resolveApparentLabelEnvironment, fromRepo label.CanonicalRepo, toApparentLabel label.ApparentLabel) (label.CanonicalLabel, error) {
	if toCanonicalLabel, ok := toApparentLabel.AsCanonicalLabel(); ok {
		// Label was already canonical. Nothing to do.
		return toCanonicalLabel, nil
	}

	if toApparentRepo, ok := toApparentLabel.GetApparentRepo(); ok {
		// Label is prefixed with an apparent repo. Resolve the repo.
		v := e.GetCanonicalRepoNameValue(&model_analysis_pb.CanonicalRepoName_Key{
			FromCanonicalRepo: fromRepo.String(),
			ToApparentRepo:    toApparentRepo.String(),
		})
		var badLabel label.CanonicalLabel
		if !v.IsSet() {
			return badLabel, evaluation.ErrMissingDependency
		}
		toCanonicalRepo, err := label.NewCanonicalRepo(v.Message.ToCanonicalRepo)
		if err != nil {
			return badLabel, fmt.Errorf("invalid canonical repo name %#v: %w", v.Message.ToCanonicalRepo, err)
		}
		return toApparentLabel.WithCanonicalRepo(toCanonicalRepo), nil
	}

	// Label is prefixed with "@@". Resolve to the root module.
	v := e.GetRootModuleValue(&model_analysis_pb.RootModule_Key{})
	var badLabel label.CanonicalLabel
	if !v.IsSet() {
		return badLabel, evaluation.ErrMissingDependency
	}
	rootModule, err := label.NewModule(v.Message.RootModuleName)
	if err != nil {
		return badLabel, fmt.Errorf("invalid root module name %#v: %w", v.Message.RootModuleName, err)
	}
	return toApparentLabel.WithCanonicalRepo(rootModule.ToModuleInstance(nil).GetBareCanonicalRepo()), nil
}

type loadBzlGlobalsEnvironment interface {
	resolveApparentLabelEnvironment
	GetBuiltinsModuleNamesValue(key *model_analysis_pb.BuiltinsModuleNames_Key) model_core.Message[*model_analysis_pb.BuiltinsModuleNames_Value]
	GetCompiledBzlFileDecodedGlobalsValue(key *model_analysis_pb.CompiledBzlFileDecodedGlobals_Key) (starlark.StringDict, bool)
}

func (c *baseComputer) loadBzlGlobals(e loadBzlGlobalsEnvironment, canonicalPackage label.CanonicalPackage, loadLabelStr string, builtinsModuleNames []string) (starlark.StringDict, error) {
	allBuiltinsModulesNames := e.GetBuiltinsModuleNamesValue(&model_analysis_pb.BuiltinsModuleNames_Key{})
	if !allBuiltinsModulesNames.IsSet() {
		return nil, evaluation.ErrMissingDependency
	}
	apparentLoadLabel, err := canonicalPackage.AppendLabel(loadLabelStr)
	if err != nil {
		return nil, fmt.Errorf("invalid label %#v in load() statement: %w", loadLabelStr, err)
	}
	canonicalRepo := canonicalPackage.GetCanonicalRepo()
	canonicalLoadLabel, err := resolveApparentLabel(e, canonicalRepo, apparentLoadLabel)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve label %#v in load() statement: %w", apparentLoadLabel.String(), err)
	}
	decodedGlobals, ok := e.GetCompiledBzlFileDecodedGlobalsValue(&model_analysis_pb.CompiledBzlFileDecodedGlobals_Key{
		Label:               canonicalLoadLabel.String(),
		BuiltinsModuleNames: builtinsModuleNames,
	})
	if !ok {
		return nil, evaluation.ErrMissingDependency
	}
	return decodedGlobals, nil
}

func (c *baseComputer) loadBzlGlobalsInStarlarkThread(e loadBzlGlobalsEnvironment, thread *starlark.Thread, loadLabelStr string, builtinsModuleNames []string) (starlark.StringDict, error) {
	return c.loadBzlGlobals(e, label.MustNewCanonicalLabel(thread.CallFrame(0).Pos.Filename()).GetCanonicalPackage(), loadLabelStr, builtinsModuleNames)
}

func (c *baseComputer) preloadBzlGlobals(e loadBzlGlobalsEnvironment, canonicalPackage label.CanonicalPackage, program *starlark.Program, builtinsModuleNames []string) (aggregateErr error) {
	numLoads := program.NumLoads()
	for i := 0; i < numLoads; i++ {
		loadLabelStr, _ := program.Load(i)
		if _, err := c.loadBzlGlobals(e, canonicalPackage, loadLabelStr, builtinsModuleNames); err != nil {
			if !errors.Is(err, evaluation.ErrMissingDependency) {
				return err
			}
			aggregateErr = err
		}
	}
	return
}

type getBzlFileBuiltinsEnvironment interface {
	GetCompiledBzlFileDecodedGlobalsValue(key *model_analysis_pb.CompiledBzlFileDecodedGlobals_Key) (starlark.StringDict, bool)
}

func (c *baseComputer) getBzlFileBuiltins(e getBzlFileBuiltinsEnvironment, builtinsModuleNames []string, baseBuiltins starlark.StringDict, dictName string) (starlark.StringDict, error) {
	allBuiltins := starlark.StringDict{}
	for name, value := range baseBuiltins {
		allBuiltins[name] = value
	}
	missingDependencies := false
	for i, builtinsModuleName := range builtinsModuleNames {
		exportsFile := fmt.Sprintf("@@%s+//:exports.bzl", builtinsModuleName)
		if globals, gotGlobals := e.GetCompiledBzlFileDecodedGlobalsValue(&model_analysis_pb.CompiledBzlFileDecodedGlobals_Key{
			Label:               exportsFile,
			BuiltinsModuleNames: builtinsModuleNames[:i],
		}); gotGlobals {
			exportedToplevels, ok := globals[dictName].(starlark.IterableMapping)
			if !ok {
				return nil, fmt.Errorf("file %#v does not declare exported_toplevels", exportsFile)
			}
			for name, value := range starlark.Entries(exportedToplevels) {
				nameStr, ok := starlark.AsString(name)
				if !ok {
					return nil, fmt.Errorf("file %#v exports builtins with non-string names", exportsFile)
				}
				allBuiltins[strings.TrimPrefix(nameStr, "+")] = value
			}
		} else {
			missingDependencies = true
		}
	}
	if missingDependencies {
		return nil, evaluation.ErrMissingDependency
	}
	return allBuiltins, nil
}

type starlarkThreadEnvironment interface {
	loadBzlGlobalsEnvironment
	GetCompiledBzlFileFunctionFactoryValue(*model_analysis_pb.CompiledBzlFileFunctionFactory_Key) (*starlark.FunctionFactory, bool)
}

// trimBuiltinModuleNames truncates the list of built-in module names up
// to a provided module name. This needs to be called when attempting to
// load() files belonging to a built-in module, so that evaluating code
// belonging to the built-in module does not result into cycles.
func trimBuiltinModuleNames(builtinsModuleNames []string, module label.Module) []string {
	moduleStr := module.String()
	i := 0
	for i < len(builtinsModuleNames) && builtinsModuleNames[i] != moduleStr {
		i++
	}
	return builtinsModuleNames[:i]
}

func (c *baseComputer) newStarlarkThread(ctx context.Context, e starlarkThreadEnvironment, builtinsModuleNames []string) *starlark.Thread {
	thread := &starlark.Thread{
		// TODO: Provide print method.
		Print: nil,
		Load: func(thread *starlark.Thread, loadLabelStr string) (starlark.StringDict, error) {
			return c.loadBzlGlobalsInStarlarkThread(e, thread, loadLabelStr, builtinsModuleNames)
		},
		Steps: 1000,
	}

	thread.SetLocal(model_starlark.CanonicalRepoResolverKey, func(fromCanonicalRepo label.CanonicalRepo, toApparentRepo label.ApparentRepo) (label.CanonicalRepo, error) {
		v := e.GetCanonicalRepoNameValue(&model_analysis_pb.CanonicalRepoName_Key{
			FromCanonicalRepo: fromCanonicalRepo.String(),
			ToApparentRepo:    toApparentRepo.String(),
		})
		var badRepo label.CanonicalRepo
		if !v.IsSet() {
			return badRepo, evaluation.ErrMissingDependency
		}
		return label.NewCanonicalRepo(v.Message.ToCanonicalRepo)
	})

	thread.SetLocal(model_starlark.RootModuleResolverKey, func() (label.Module, error) {
		v := e.GetRootModuleValue(&model_analysis_pb.RootModule_Key{})
		var badModule label.Module
		if !v.IsSet() {
			return badModule, evaluation.ErrMissingDependency
		}
		return label.NewModule(v.Message.RootModuleName)
	})

	valueDecodingOptions := c.getValueDecodingOptions(ctx, func(canonicalLabel label.CanonicalLabel) (starlark.Value, error) {
		return model_starlark.NewLabel(canonicalLabel), nil
	})
	thread.SetLocal(model_starlark.FunctionFactoryResolverKey, func(filename label.CanonicalLabel) (*starlark.FunctionFactory, *model_starlark.ValueDecodingOptions, error) {
		// Prevent modules containing builtin Starlark code from
		// depending on itself.
		functionFactory, gotFunctionFactory := e.GetCompiledBzlFileFunctionFactoryValue(&model_analysis_pb.CompiledBzlFileFunctionFactory_Key{
			Label:               filename.String(),
			BuiltinsModuleNames: trimBuiltinModuleNames(builtinsModuleNames, filename.GetCanonicalRepo().GetModuleInstance().GetModule()),
		})
		if !gotFunctionFactory {
			return nil, nil, evaluation.ErrMissingDependency
		}
		return functionFactory, valueDecodingOptions, nil
	})
	return thread
}

func (c *baseComputer) ComputeBuildResultValue(ctx context.Context, key *model_analysis_pb.BuildResult_Key, e BuildResultEnvironment) (PatchedBuildResultValue, error) {
	// TODO: Do something proper here.
	missing := false
	for _, pkg := range []string{
		"//cmd/bb_copy",
		"//cmd/bb_replicator",
		"//cmd/bb_storage",
		"//internal/mock",
		"//internal/mock/aliases",
		"//pkg/auth",
		"//pkg/blobstore",
		"//pkg/blobstore/buffer",
		"//pkg/blobstore/completenesschecking",
		"//pkg/blobstore/configuration",
		"//pkg/blobstore/grpcclients",
		"//pkg/blobstore/grpcservers",
		"//pkg/blobstore/local",
		"//pkg/blobstore/mirrored",
		"//pkg/blobstore/readcaching",
		"//pkg/blobstore/readfallback",
		"//pkg/blobstore/replication",
		"//pkg/blobstore/sharding",
		"//pkg/blobstore/slicing",
		"//pkg/blockdevice",
		"//pkg/builder",
		"//pkg/capabilities",
		"//pkg/clock",
		"//pkg/cloud/aws",
		"//pkg/cloud/gcp",
		"//pkg/digest",
		"//pkg/digest/sha256tree",
		"//pkg/eviction",
		"//pkg/filesystem",
		"//pkg/filesystem/path",
		"//pkg/filesystem/windowsext",
		"//pkg/global",
		"//pkg/grpc",
		"//pkg/http",
		"//pkg/jwt",
		"//pkg/otel",
		"//pkg/program",
		"//pkg/prometheus",
		"//pkg/proto/auth",
		"//pkg/proto/blobstore/local",
		"//pkg/proto/configuration/auth",
		"//pkg/proto/configuration/bb_copy",
		"//pkg/proto/configuration/bb_replicator",
		"//pkg/proto/configuration/bb_storage",
		"//pkg/proto/configuration/blobstore",
		"//pkg/proto/configuration/blockdevice",
		"//pkg/proto/configuration/builder",
		"//pkg/proto/configuration/cloud/aws",
		"//pkg/proto/configuration/cloud/gcp",
		"//pkg/proto/configuration/digest",
		"//pkg/proto/configuration/eviction",
		"//pkg/proto/configuration/global",
		"//pkg/proto/configuration/grpc",
		"//pkg/proto/configuration/http",
		"//pkg/proto/configuration/jwt",
		"//pkg/proto/configuration/tls",
		"//pkg/proto/fsac",
		"//pkg/proto/http/oidc",
		"//pkg/proto/icas",
		"//pkg/proto/iscc",
		"//pkg/proto/replicator",
		"//pkg/random",
		"//pkg/testutil",
		"//pkg/util",
	} {
		targetCompletion := e.GetTargetCompletionValue(&model_analysis_pb.TargetCompletion_Key{
			Label: "@@com_github_buildbarn_bb_storage+" + pkg,
		})
		if !targetCompletion.IsSet() {
			missing = true
		}
	}
	if missing {
		return PatchedBuildResultValue{}, evaluation.ErrMissingDependency
	}
	return model_core.NewSimplePatchedMessage[dag.ObjectContentsWalker](&model_analysis_pb.BuildResult_Value{}), nil
}

func (c *baseComputer) ComputeBuildSpecificationValue(ctx context.Context, key *model_analysis_pb.BuildSpecification_Key, e BuildSpecificationEnvironment) (PatchedBuildSpecificationValue, error) {
	reader := model_parser.NewStorageBackedParsedObjectReader(
		c.objectDownloader,
		c.buildSpecificationEncoder,
		model_parser.NewMessageObjectParser[object.LocalReference, model_build_pb.BuildSpecification](),
	)
	buildSpecification, _, err := reader.ReadParsedObject(ctx, c.buildSpecificationReference)
	if err != nil {
		return PatchedBuildSpecificationValue{}, err
	}

	patchedBuildSpecification := model_core.NewPatchedMessageFromExisting(
		buildSpecification,
		func(index int) dag.ObjectContentsWalker {
			return dag.ExistingObjectContentsWalker
		},
	)
	return PatchedBuildSpecificationValue{
		Message: &model_analysis_pb.BuildSpecification_Value{
			BuildSpecification: patchedBuildSpecification.Message,
		},
		Patcher: patchedBuildSpecification.Patcher,
	}, nil
}

func (c *baseComputer) ComputeBuiltinsModuleNamesValue(ctx context.Context, key *model_analysis_pb.BuiltinsModuleNames_Key, e BuiltinsModuleNamesEnvironment) (PatchedBuiltinsModuleNamesValue, error) {
	buildSpecification := e.GetBuildSpecificationValue(&model_analysis_pb.BuildSpecification_Key{})
	if !buildSpecification.IsSet() {
		return PatchedBuiltinsModuleNamesValue{}, evaluation.ErrMissingDependency
	}
	return model_core.NewSimplePatchedMessage[dag.ObjectContentsWalker](&model_analysis_pb.BuiltinsModuleNames_Value{
		BuiltinsModuleNames: buildSpecification.Message.BuildSpecification.GetBuiltinsModuleNames(),
	}), nil
}

func (c *baseComputer) ComputeDirectoryAccessParametersValue(ctx context.Context, key *model_analysis_pb.DirectoryAccessParameters_Key, e DirectoryAccessParametersEnvironment) (PatchedDirectoryAccessParametersValue, error) {
	buildSpecification := e.GetBuildSpecificationValue(&model_analysis_pb.BuildSpecification_Key{})
	if !buildSpecification.IsSet() {
		return PatchedDirectoryAccessParametersValue{}, evaluation.ErrMissingDependency
	}
	return model_core.NewSimplePatchedMessage[dag.ObjectContentsWalker](&model_analysis_pb.DirectoryAccessParameters_Value{
		DirectoryAccessParameters: buildSpecification.Message.BuildSpecification.GetDirectoryCreationParameters().GetAccess(),
	}), nil
}

func (c *baseComputer) ComputeFilePropertiesValue(ctx context.Context, key *model_analysis_pb.FileProperties_Key, e FilePropertiesEnvironment) (PatchedFilePropertiesValue, error) {
	repoValue := e.GetRepoValue(&model_analysis_pb.Repo_Key{
		CanonicalRepo: key.CanonicalRepo,
	})
	directoryAccessParametersValue := e.GetDirectoryAccessParametersValue(&model_analysis_pb.DirectoryAccessParameters_Key{})
	if !repoValue.IsSet() {
		return PatchedFilePropertiesValue{}, evaluation.ErrMissingDependency
	}

	if !directoryAccessParametersValue.IsSet() {
		return PatchedFilePropertiesValue{}, evaluation.ErrMissingDependency
	}
	directoryAccessParameters, err := model_filesystem.NewDirectoryAccessParametersFromProto(
		directoryAccessParametersValue.Message.DirectoryAccessParameters,
		c.buildSpecificationReference.GetReferenceFormat(),
	)
	if err != nil {
		return PatchedFilePropertiesValue{}, fmt.Errorf("invalid directory access parameters: %w", err)
	}
	directoryReader := model_parser.NewStorageBackedParsedObjectReader(
		c.objectDownloader,
		directoryAccessParameters.GetEncoder(),
		model_parser.NewMessageObjectParser[object.LocalReference, model_filesystem_pb.Directory](),
	)
	leavesReader := model_parser.NewStorageBackedParsedObjectReader(
		c.objectDownloader,
		directoryAccessParameters.GetEncoder(),
		model_parser.NewMessageObjectParser[object.LocalReference, model_filesystem_pb.Leaves](),
	)

	rootDirectoryReferenceIndex, err := model_core.GetIndexFromReferenceMessage(repoValue.Message.RootDirectoryReference, repoValue.OutgoingReferences.GetDegree())
	if err != nil {
		return PatchedFilePropertiesValue{}, fmt.Errorf("invalid root directory reference: %w", err)
	}

	resolver := model_filesystem.NewDirectoryMerkleTreeFileResolver(
		ctx,
		directoryReader,
		leavesReader,
		repoValue.OutgoingReferences.GetOutgoingReference(rootDirectoryReferenceIndex),
	)
	if err := bb_path.Resolve(
		bb_path.UNIXFormat.NewParser(key.Path),
		bb_path.NewLoopDetectingScopeWalker(
			bb_path.NewRelativeScopeWalker(resolver),
		),
	); err != nil {
		if status.Code(err) == codes.NotFound {
			return model_core.NewSimplePatchedMessage[dag.ObjectContentsWalker](&model_analysis_pb.FileProperties_Value{}), nil
		}
		return PatchedFilePropertiesValue{}, fmt.Errorf("failed to resolve %#v: %w", key.Path, err)
	}

	fileProperties := resolver.GetFileProperties()
	if !fileProperties.IsSet() {
		return PatchedFilePropertiesValue{}, errors.New("path resolves to a directory")
	}
	patchedFileProperties := model_core.NewPatchedMessageFromExisting(
		fileProperties,
		func(index int) dag.ObjectContentsWalker {
			return dag.ExistingObjectContentsWalker
		},
	)
	return PatchedFilePropertiesValue{
		Message: &model_analysis_pb.FileProperties_Value{
			Exists: patchedFileProperties.Message,
		},
		Patcher: patchedFileProperties.Patcher,
	}, nil
}

func (c *baseComputer) ComputeFileReaderValue(ctx context.Context, key *model_analysis_pb.FileReader_Key, e FileReaderEnvironment) (*model_filesystem.FileReader, error) {
	fileAccessParametersValue := e.GetFileAccessParametersValue(&model_analysis_pb.FileAccessParameters_Key{})
	if !fileAccessParametersValue.IsSet() {
		return nil, evaluation.ErrMissingDependency
	}
	fileAccessParameters, err := model_filesystem.NewFileAccessParametersFromProto(
		fileAccessParametersValue.Message.FileAccessParameters,
		c.buildSpecificationReference.GetReferenceFormat(),
	)
	if err != nil {
		return nil, fmt.Errorf("invalid directory access parameters: %w", err)
	}
	fileContentsListReader := model_parser.NewStorageBackedParsedObjectReader(
		c.objectDownloader,
		fileAccessParameters.GetFileContentsListEncoder(),
		model_filesystem.NewFileContentsListObjectParser[object.LocalReference](),
	)
	fileChunkReader := model_parser.NewStorageBackedParsedObjectReader(
		c.objectDownloader,
		fileAccessParameters.GetChunkEncoder(),
		model_parser.NewRawObjectParser[object.LocalReference](),
	)
	return model_filesystem.NewFileReader(fileContentsListReader, fileChunkReader), nil
}

func (c *baseComputer) ComputeRepoDefaultAttrsValue(ctx context.Context, key *model_analysis_pb.RepoDefaultAttrs_Key, e RepoDefaultAttrsEnvironment) (PatchedRepoDefaultAttrsValue, error) {
	canonicalRepo, err := label.NewCanonicalRepo(key.CanonicalRepo)
	if err != nil {
		return PatchedRepoDefaultAttrsValue{}, fmt.Errorf("invalid canonical repo: %w", err)
	}

	repoFileName := label.MustNewTargetName("REPO.bazel")
	repoFileProperties := e.GetFilePropertiesValue(&model_analysis_pb.FileProperties_Key{
		CanonicalRepo: canonicalRepo.String(),
		Path:          repoFileName.String(),
	})

	fileReader, gotFileReader := e.GetFileReaderValue(&model_analysis_pb.FileReader_Key{})
	if !repoFileProperties.IsSet() || !gotFileReader {
		return PatchedRepoDefaultAttrsValue{}, evaluation.ErrMissingDependency
	}

	// Read the contents of REPO.bazel.
	repoFileLabel := canonicalRepo.GetRootPackage().AppendTargetName(repoFileName)
	if repoFileProperties.Message.Exists == nil {
		return model_core.NewSimplePatchedMessage[dag.ObjectContentsWalker](&model_analysis_pb.RepoDefaultAttrs_Value{
			InheritableAttrs: &model_starlark.DefaultInheritableAttrs,
		}), nil
	}
	referenceFormat := c.buildSpecificationReference.GetReferenceFormat()
	repoFileContentsEntry, err := model_filesystem.NewFileContentsEntryFromProto(
		model_core.Message[*model_filesystem_pb.FileContents]{
			Message:            repoFileProperties.Message.Exists.GetContents(),
			OutgoingReferences: repoFileProperties.OutgoingReferences,
		},
		referenceFormat,
	)
	if err != nil {
		return PatchedRepoDefaultAttrsValue{}, fmt.Errorf("invalid contents for file %#v: %w", repoFileLabel.String(), err)
	}
	repoFileData, err := fileReader.FileReadAll(ctx, repoFileContentsEntry, 1<<20)
	if err != nil {
		return PatchedRepoDefaultAttrsValue{}, err
	}

	// Extract the default inheritable attrs from REPO.bazel.
	defaultAttrs, err := model_starlark.ParseRepoDotBazel(
		string(repoFileData),
		canonicalRepo.GetRootPackage().AppendTargetName(repoFileName),
		c.getInlinedTreeOptions(),
	)
	if err != nil {
		return PatchedRepoDefaultAttrsValue{}, fmt.Errorf("failed to parse %#v: %w", repoFileLabel.String(), err)
	}

	return model_core.PatchedMessage[*model_analysis_pb.RepoDefaultAttrs_Value, dag.ObjectContentsWalker]{
		Message: &model_analysis_pb.RepoDefaultAttrs_Value{
			InheritableAttrs: defaultAttrs.Message,
		},
		Patcher: defaultAttrs.Patcher,
	}, nil
}

func (c *baseComputer) ComputeTargetCompletionValue(ctx context.Context, key *model_analysis_pb.TargetCompletion_Key, e TargetCompletionEnvironment) (PatchedTargetCompletionValue, error) {
	configuredTarget := e.GetConfiguredTargetValue(&model_analysis_pb.ConfiguredTarget_Key{
		Label: key.Label,
	})
	if !configuredTarget.IsSet() {
		return PatchedTargetCompletionValue{}, evaluation.ErrMissingDependency
	}
	return model_core.NewSimplePatchedMessage[dag.ObjectContentsWalker](&model_analysis_pb.TargetCompletion_Value{}), nil
}

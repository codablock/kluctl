package deployment

import (
	"fmt"
	"github.com/codablock/kluctl/pkg/jinja2"
	"github.com/codablock/kluctl/pkg/k8s"
	"github.com/codablock/kluctl/pkg/types"
	"github.com/codablock/kluctl/pkg/utils"
	"github.com/codablock/kluctl/pkg/utils/uo"
	log "github.com/sirupsen/logrus"
	"path/filepath"
	"strings"
	"sync"
)

var kustomizeDirsDeprecatedOnce sync.Once
var includesDeprecatedOnce sync.Once

type DeploymentProject struct {
	VarsCtx          *jinja2.VarsCtx
	dir              string
	sealedSecretsDir string

	config types.DeploymentProjectConfig

	includes map[int]*DeploymentProject

	parentProject        *DeploymentProject
	parentProjectInclude *types.DeploymentItemConfig
}

func NewDeploymentProject(k *k8s.K8sCluster, varsCtx *jinja2.VarsCtx, dir string, sealedSecretsDir string, parentProject *DeploymentProject) (*DeploymentProject, error) {
	dp := &DeploymentProject{
		VarsCtx:          varsCtx.Copy(),
		dir:              dir,
		sealedSecretsDir: sealedSecretsDir,
		parentProject:    parentProject,
		includes:         map[int]*DeploymentProject{},
	}

	if !utils.IsDirectory(dir) {
		return nil, fmt.Errorf("%s does not exist or is not a directory", dir)
	}

	err := dp.loadConfig(k)
	if err != nil {
		return nil, fmt.Errorf("failed to load deployment config for %s: %w", dir, err)
	}

	err = dp.loadIncludes(k)
	if err != nil {
		return nil, fmt.Errorf("failed to load includes for %s: %w", dir, err)
	}

	return dp, nil
}

func (p *DeploymentProject) MergeSecretsIntoAllChildren(vars *uo.UnstructuredObject) {
	for _, c := range p.getChildren(true, true) {
		c.VarsCtx.UpdateChild("secrets", vars)
	}
}

func (p *DeploymentProject) loadConfig(k *k8s.K8sCluster) error {
	configPath := filepath.Join(p.dir, "deployment.yml")
	if !utils.Exists(configPath) {
		if utils.Exists(filepath.Join(p.dir, "kustomization.yml")) {
			return fmt.Errorf("deployment.yml not found but folder %s contains a kustomization.yml", p.dir)
		}
		return fmt.Errorf("%s not found", p.dir)
	}

	err := p.VarsCtx.RenderYamlFile("deployment.yml", p.getRenderSearchDirs(), &p.config)
	if err != nil {
		return fmt.Errorf("failed to load deployment.yml: %w", err)
	}

	err = p.VarsCtx.LoadVarsList(k, p.getRenderSearchDirs(), p.config.Vars)
	if err != nil {
		return fmt.Errorf("failed to load deployment.yml vars: %w", err)
	}

	// TODO remove obsolete code
	if len(p.config.Deployments) != 0 && (len(p.config.KustomizeDirs) != 0 || len(p.config.Includes) != 0) {
		return fmt.Errorf("using 'deployments' and 'kustomizeDirs' at the same time is not allowed")
	}
	if len(p.config.KustomizeDirs) != 0 {
		kustomizeDirsDeprecatedOnce.Do(func() {
			log.Warningf("'kustomizeDirs' is deprecated, use 'deployments' instead")
		})
		p.config.Deployments = p.config.KustomizeDirs
		p.config.KustomizeDirs = nil
	}
	if len(p.config.Includes) != 0 {
		includesDeprecatedOnce.Do(func() {
			log.Warningf("'includes' is deprecated, use 'deployments' instead")
		})
		for _, inc := range p.config.Includes {
			c := *inc
			c.Include = c.Path
			c.Path = nil
			p.config.Deployments = append(p.config.Deployments, &c)
		}
		p.config.Includes = nil
	}

	// If there are no explicit tags set, interpret the path as a tag, which allows to
	// enable/disable single deployments via included/excluded tags
	for _, item := range p.config.Deployments {
		if len(item.Tags) != 0 {
			continue
		}
		if item.Path != nil {
			item.Tags = []string{filepath.Base(*item.Path)}
		} else if item.Include != nil {
			item.Tags = []string{filepath.Base(*item.Include)}
		}
	}

	err = p.checkDeploymentDirs()
	if err != nil {
		return err
	}

	if len(p.getCommonLabels()) == 0 {
		return fmt.Errorf("no commonLabels/deleteByLabels in root deployment. This is not allowed")
	}
	if len(p.config.Args) != 0 && p.parentProject != nil {
		return fmt.Errorf("only the root deployment.yml can define args")
	}
	return nil
}

func (p *DeploymentProject) checkDeploymentDirs() error {
	rootProject := p.getRootProject()
	for _, di := range p.config.Deployments {
		if di.Path == nil && di.Include == nil {
			continue
		}

		var pth string
		if di.Path != nil {
			pth = *di.Path
		} else {
			pth = *di.Include
		}

		diDir := filepath.Join(p.dir, pth)
		diDir, err := filepath.Abs(diDir)
		if err != nil {
			return err
		}

		if !strings.HasPrefix(diDir, rootProject.dir) {
			return fmt.Errorf("path/include is not part of root deployment project: %s", pth)
		}

		if !utils.Exists(diDir) {
			return fmt.Errorf("deployment directory does not exist: %s", pth)
		}
		if !utils.IsDirectory(diDir) {
			return fmt.Errorf("deployment path is not a directory: %s", pth)
		}

		if di.Path != nil {
			pth = filepath.Join(diDir, "kustomization.yml")
		} else {
			pth = filepath.Join(diDir, "deployment.yml")
		}
		if !utils.IsFile(pth) {
			return fmt.Errorf("%s not found or not a file", pth)
		}
	}
	return nil
}

func (p *DeploymentProject) loadIncludes(k *k8s.K8sCluster) error {
	for i, inc := range p.config.Deployments {
		if inc.Include == nil {
			continue
		}

		incDir := filepath.Join(p.dir, *inc.Include)

		varsCtx := p.VarsCtx.Copy()
		err := varsCtx.LoadVarsList(k, p.getRenderSearchDirs(), inc.Vars)
		if err != nil {
			return err
		}

		newProject, err := NewDeploymentProject(k, varsCtx, incDir, p.sealedSecretsDir, p)
		if err != nil {
			return err
		}
		newProject.parentProjectInclude = inc

		p.includes[i] = newProject
	}
	return nil
}

func (p *DeploymentProject) getSealedSecretsDir() *string {
	root := p.getRootProject()
	if root.config.SealedSecrets == nil {
		return nil
	}
	return root.config.SealedSecrets.OutputPattern
}

func (p *DeploymentProject) getRootProject() *DeploymentProject {
	if p.parentProject == nil {
		return p
	}
	return p.parentProject.getRootProject()
}

type deploymentProjectAndIncludeItem struct {
	p   *DeploymentProject
	inc *types.DeploymentItemConfig
}

func (p *DeploymentProject) getParents() []deploymentProjectAndIncludeItem {
	var parents []deploymentProjectAndIncludeItem
	var inc *types.DeploymentItemConfig
	d := p
	for d != nil {
		parents = append(parents, deploymentProjectAndIncludeItem{p: d, inc: inc})
		inc = d.parentProjectInclude
		d = d.parentProject
	}
	return parents
}

func (p *DeploymentProject) getChildren(recursive bool, includeSelf bool) []*DeploymentProject {
	var children []*DeploymentProject
	if includeSelf {
		children = append(children, p)
	}
	for _, d := range p.includes {
		children = append(children, d)
		if recursive {
			children = append(children, d.getChildren(true, false)...)
		}
	}
	return children
}

func (p *DeploymentProject) getRenderSearchDirs() []string {
	var ret []string
	for _, d := range p.getParents() {
		ret = append(ret, d.p.dir)
	}
	return ret
}

func (p *DeploymentProject) getCommonLabels() map[string]string {
	ret := make(map[string]string)
	parents := p.getParents()
	for i, _ := range parents {
		d := parents[len(parents)-i-1]
		uo.MergeStrMap(ret, d.p.config.CommonLabels)
	}
	return ret
}

func (p *DeploymentProject) getDeleteByLabels() map[string]string {
	ret := make(map[string]string)
	parents := p.getParents()
	for i, _ := range parents {
		d := parents[len(parents)-i-1]
		uo.MergeStrMap(ret, d.p.config.DeleteByLabels)
	}
	return ret
}

func (p *DeploymentProject) getOverrideNamespace() *string {
	for _, e := range p.getParents() {
		if e.p.config.OverrideNamespace != nil {
			return e.p.config.OverrideNamespace
		}
	}
	return nil
}

func (p *DeploymentProject) getTags() *utils.OrderedMap {
	var tags utils.OrderedMap
	for _, e := range p.getParents() {
		if e.inc != nil {
			for _, t := range e.inc.Tags {
				tags.Set(t, true)
			}
		}
		for _, t := range e.p.config.Tags {
			tags.Set(t, true)
		}
	}
	return &tags
}

func (p *DeploymentProject) getIgnoreForDiffs(ignoreTags, ignoreLabels, ignoreAnnotations bool) []*types.IgnoreForDiffItemConfig {
	var ret []*types.IgnoreForDiffItemConfig
	for _, e := range p.getParents() {
		ret = append(ret, e.p.config.IgnoreForDiff...)
	}
	if ignoreTags {
		ret = append(ret, &types.IgnoreForDiffItemConfig{FieldPath: []string{`metadata.labels."kluctl.io/tag-*"`}})
	}
	if ignoreLabels {
		ret = append(ret, &types.IgnoreForDiffItemConfig{FieldPath: []string{`metadata.labels.*`}})
	}
	if ignoreAnnotations {
		ret = append(ret, &types.IgnoreForDiffItemConfig{FieldPath: []string{`metadata.annotations.*`}})
	}
	return ret
}
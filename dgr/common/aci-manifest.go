package common

import (
	"bytes"
	"encoding/json"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/appc/spec/aci"
	"github.com/appc/spec/schema"
	"github.com/appc/spec/schema/types"
	"github.com/n0rad/go-erlog/data"
	"github.com/n0rad/go-erlog/errs"
)

func ExtractManifestContentFromAci(aciPath string) ([]byte, error) {
	fields := data.WithField("file", aciPath)
	input, err := os.Open(aciPath)
	if err != nil {
		return nil, errs.WithEF(err, fields, "Cannot open file")
	}
	defer input.Close()

	tr, err := aci.NewCompressedTarReader(input)
	if err != nil {
		return nil, errs.WithEF(err, fields, "Cannot open file as tar")
	}

Tar:
	for {
		hdr, err := tr.Next()
		switch err {
		case io.EOF:
			break Tar
		case nil:
			if filepath.Clean(hdr.Name) == aci.ManifestFile {
				bytes, err := ioutil.ReadAll(tr)
				if err != nil {
					return nil, errs.WithEF(err, fields, "Cannot read manifest content in tar")
				}
				return bytes, nil
			}
		default:
			return nil, errs.WithEF(err, fields, "error reading tarball file")
		}
	}
	return nil, errs.WithEF(err, fields, "Cannot found manifest in file")
}

func ExtractManifestFromAci(aciPath string) (*schema.ImageManifest, error) {
	fields := data.WithField("file", aciPath)
	content, err := ExtractManifestContentFromAci(aciPath)
	if err != nil {
		return nil, errs.WithEF(err, fields, "Cannot extract aci manifest content from file")
	}
	im := &schema.ImageManifest{}
	if err = im.UnmarshalJSON(content); err != nil {
		return nil, errs.WithEF(err, fields.WithField("content", string(content)), "Cannot unmarshall json content")
	}
	return im, nil
}

func ExtractNameVersionFromManifest(im *schema.ImageManifest) *ACFullname {
	name := string(im.Name)
	if val, ok := im.Labels.Get("version"); ok {
		name += ":" + val
	}
	return NewACFullName(name)
}

// prettifyJSON improves legibility of a JSON manifest file
// by inserting and removing whitespace.
func prettifyJSON(content []byte) []byte {
	re := regexp.MustCompile(`{\s*("name":[^\n]*),\n\s*([^\n]+)\n\s*}`)
	t := re.ReplaceAll(content, []byte("{$1, $2}"))
	return t
}

func WriteAciManifest(m *AciManifest, targetFile string, projectName string, dgrVersion string) error {
	fields := data.WithField("name", m.NameAndVersion.String())
	name, err := types.NewACIdentifier(projectName)
	if err != nil {
		return errs.WithEF(err, fields, "aci name is not a valid identifier for rkt")
	}

	labels := types.Labels{}
	if m.NameAndVersion.Version() != "" {
		labels = append(labels, types.Label{Name: "version", Value: m.NameAndVersion.Version()})
	}
	labels = append(labels, types.Label{Name: "os", Value: "linux"})
	labels = append(labels, types.Label{Name: "arch", Value: "amd64"})

	if m.Aci.App.User == "" {
		m.Aci.App.User = "0"
	}
	if m.Aci.App.Group == "" {
		m.Aci.App.Group = "0"
	}

	im := schema.BlankImageManifest()
	im.Annotations = m.Aci.Annotations

	if _, ok := im.Annotations.Get("build-date"); !ok {
		buildDateIdentifier, _ := types.NewACIdentifier("build-date")
		im.Annotations.Set(*buildDateIdentifier, time.Now().Format(time.RFC3339))
	}
	im.Dependencies, err = ToAppcDependencies(m.Aci.Dependencies)
	if err != nil {
		return errs.WithEF(err, fields, "Failed to prepare dependencies for manifest")
	}
	im.Name = *name
	im.Labels = labels

	for _, exclusion := range m.Build.Exclude {
		if strings.HasPrefix("/dgr/bin", exclusion) {
			goto collectionIsComplete
		}
	}
	if len(m.Aci.App.Exec) == 0 {
		m.Aci.App.Exec = []string{"/dgr/bin/busybox", "sh"}
	}
	// Set a pre-start handler, to run dgr's scripts.
	for i := range m.Aci.App.EventHandlers {
		if m.Aci.App.EventHandlers[i].Name == "pre-start" {
			goto collectionIsComplete
		}
	}
	m.Aci.App.EventHandlers = append(m.Aci.App.EventHandlers,
		types.EventHandler{Name: "pre-start", Exec: []string{"/dgr/bin/prestart"}})

collectionIsComplete:

	im.App = &types.App{
		Exec:              m.Aci.App.Exec,
		EventHandlers:     m.Aci.App.EventHandlers,
		User:              m.Aci.App.User,
		Group:             m.Aci.App.Group,
		WorkingDirectory:  m.Aci.App.WorkingDirectory,
		SupplementaryGIDs: m.Aci.App.SupplementaryGIDs,
		Environment:       m.Aci.App.Environment,
		MountPoints:       m.Aci.App.MountPoints,
		Ports:             m.Aci.App.Ports,
		Isolators:         m.Aci.App.Isolators,
	}

	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if enc.Encode(im) != nil {
		return errs.WithEF(err, fields.WithField("object", im), "Failed to marshal manifest")
	}
	buff := prettifyJSON(b.Bytes())
	err = ioutil.WriteFile(targetFile, buff, 0644)
	if err != nil {
		return errs.WithEF(err, fields.WithField("file", targetFile), "Failed to write manifest file")
	}
	return nil
}

func ToAppcDependencies(dependencies []ACFullname) (types.Dependencies, error) {
	appcDependencies := types.Dependencies{}
	for _, dep := range dependencies {
		id, err := types.NewACIdentifier(dep.Name())
		if err != nil {
			return nil, errs.WithEF(err, data.WithField("name", dep.Name()), "invalid identifer name for rkt")
		}
		t := types.Dependency{ImageName: *id}
		if dep.Version() != "" {
			t.Labels = types.Labels{}
			t.Labels = append(t.Labels, types.Label{Name: "version", Value: dep.Version()})
		}

		appcDependencies = append(appcDependencies, t)
	}
	return appcDependencies, nil
}

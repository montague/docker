package docker

import (
	"fmt"
	"github.com/dotcloud/docker/utils"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// A Graph is a store for versioned filesystem images and the relationship between them.
type Graph struct {
	Root    string
	idIndex *utils.TruncIndex
}

// NewGraph instantiates a new graph at the given root path in the filesystem.
// `root` will be created if it doesn't exist.
func NewGraph(root string) (*Graph, error) {
	abspath, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	// Create the root directory if it doesn't exists
	if err := os.MkdirAll(root, 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}
	graph := &Graph{
		Root:    abspath,
		idIndex: utils.NewTruncIndex(),
	}
	if err := graph.restore(); err != nil {
		return nil, err
	}
	return graph, nil
}

func (graph *Graph) restore() error {
	dir, err := ioutil.ReadDir(graph.Root)
	if err != nil {
		return err
	}
	for _, v := range dir {
		id := v.Name()
		graph.idIndex.Add(id)
	}
	return nil
}

// FIXME: Implement error subclass instead of looking at the error text
// Note: This is the way golang implements os.IsNotExists on Plan9
func (graph *Graph) IsNotExist(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "No such"))
}

// Exists returns true if an image is registered at the given id.
// If the image doesn't exist or if an error is encountered, false is returned.
func (graph *Graph) Exists(id string) bool {
	if _, err := graph.Get(id); err != nil {
		return false
	}
	return true
}

// Get returns the image with the given id, or an error if the image doesn't exist.
func (graph *Graph) Get(name string) (*Image, error) {
	id, err := graph.idIndex.Get(name)
	if err != nil {
		return nil, err
	}
	// FIXME: return nil when the image doesn't exist, instead of an error
	img, err := LoadImage(graph.imageRoot(id))
	if err != nil {
		return nil, err
	}
	if img.ID != id {
		return nil, fmt.Errorf("Image stored at '%s' has wrong id '%s'", id, img.ID)
	}
	img.graph = graph
	if img.size == 0 {
		root, err := img.root()
		if err != nil {
			return nil, err
		}
		if err := StoreSize(img, root); err != nil {
			return nil, err
		}
	}
	return img, nil
}

// Create creates a new image and registers it in the graph.
func (graph *Graph) Create(layerData Archive, container *Container, author string, config *Config) (*Image, error) {
	img := &Image{
		ID:            GenerateID(),
		Created:       time.Now(),
		DockerVersion: VERSION,
		Author:        author,
		Config:        config,
	}
	if container != nil {
		img.Parent = container.Image
		img.ContainerConfig = *container.Config
	}
	if err := graph.Register(nil, layerData, img); err != nil {
		return nil, err
	}
	return img, nil
}

// Register imports a pre-existing image into the graph.
// FIXME: pass img as first argument
func (graph *Graph) Register(jsonData []byte, layerData Archive, img *Image) error {
	if err := ValidateID(img.ID); err != nil {
		return err
	}
	// (This is a convenience to save time. Race conditions are taken care of by os.Rename)
	if graph.Exists(img.ID) {
		return fmt.Errorf("Image %s already exists", img.ID)
	}
	tmp, err := graph.Mktemp("")
	defer os.RemoveAll(tmp)
	if err != nil {
		return fmt.Errorf("Mktemp failed: %s", err)
	}
	if err := StoreImage(img, jsonData, layerData, tmp); err != nil {
		return err
	}
	// Commit
	if err := os.Rename(tmp, graph.imageRoot(img.ID)); err != nil {
		return err
	}
	img.graph = graph
	graph.idIndex.Add(img.ID)
	return nil
}

// TempLayerArchive creates a temporary archive of the given image's filesystem layer.
//   The archive is stored on disk and will be automatically deleted as soon as has been read.
//   If output is not nil, a human-readable progress bar will be written to it.
//   FIXME: does this belong in Graph? How about MktempFile, let the caller use it for archives?
func (graph *Graph) TempLayerArchive(id string, compression Compression, sf *utils.StreamFormatter, output io.Writer) (*TempArchive, error) {
	image, err := graph.Get(id)
	if err != nil {
		return nil, err
	}
	tmp, err := graph.tmp()
	if err != nil {
		return nil, err
	}
	archive, err := image.TarLayer(compression)
	if err != nil {
		return nil, err
	}
	return NewTempArchive(utils.ProgressReader(ioutil.NopCloser(archive), 0, output, sf.FormatProgress("", "Buffering to disk", "%v/%v (%v)"), sf, true), tmp.Root)
}

// Mktemp creates a temporary sub-directory inside the graph's filesystem.
func (graph *Graph) Mktemp(id string) (string, error) {
	if id == "" {
		id = GenerateID()
	}
	tmp, err := graph.tmp()
	if err != nil {
		return "", fmt.Errorf("Couldn't create temp: %s", err)
	}
	if tmp.Exists(id) {
		return "", fmt.Errorf("Image %s already exists", id)
	}
	return tmp.imageRoot(id), nil
}

// getDockerInitLayer returns the path of a layer containing a mountpoint suitable
// for bind-mounting dockerinit into the container. The mountpoint is simply an
// empty file at /.dockerinit
//
// This extra layer is used by all containers as the top-most ro layer. It protects
// the container from unwanted side-effects on the rw layer.
func (graph *Graph) getDockerInitLayer() (string, error) {
	tmp, err := graph.tmp()
	if err != nil {
		return "", err
	}
	initLayer := tmp.imageRoot("_dockerinit")
	if err := os.Mkdir(initLayer, 0755); err != nil && !os.IsExist(err) {
		// If directory already existed, keep going.
		// For all other errors, abort.
		return "", err
	}

	for pth, typ := range map[string]string{
		"/dev/pts":         "dir",
		"/dev/shm":         "dir",
		"/proc":            "dir",
		"/sys":             "dir",
		"/.dockerinit":     "file",
		"/etc/resolv.conf": "file",
		// "var/run": "dir",
		// "var/lock": "dir",
	} {
		if _, err := os.Stat(path.Join(initLayer, pth)); err != nil {
			if os.IsNotExist(err) {
				switch typ {
				case "dir":
					if err := os.MkdirAll(path.Join(initLayer, pth), 0755); err != nil {
						return "", err
					}
				case "file":
					if err := os.MkdirAll(path.Join(initLayer, path.Dir(pth)), 0755); err != nil {
						return "", err
					}

					if f, err := os.OpenFile(path.Join(initLayer, pth), os.O_CREATE, 0755); err != nil {
						return "", err
					} else {
						f.Close()
					}
				}
			} else {
				return "", err
			}
		}
	}

	// Layer is ready to use, if it wasn't before.
	return initLayer, nil
}

func (graph *Graph) tmp() (*Graph, error) {
	// Changed to _tmp from :tmp:, because it messed with ":" separators in aufs branch syntax...
	return NewGraph(path.Join(graph.Root, "_tmp"))
}

// Check if given error is "not empty".
// Note: this is the way golang does it internally with os.IsNotExists.
func isNotEmpty(err error) bool {
	switch pe := err.(type) {
	case nil:
		return false
	case *os.PathError:
		err = pe.Err
	case *os.LinkError:
		err = pe.Err
	}
	return strings.Contains(err.Error(), " not empty")
}

// Delete atomically removes an image from the graph.
func (graph *Graph) Delete(name string) error {
	id, err := graph.idIndex.Get(name)
	if err != nil {
		return err
	}
	tmp, err := graph.Mktemp("")
	if err != nil {
		return err
	}
	graph.idIndex.Delete(id)
	err = os.Rename(graph.imageRoot(id), tmp)
	if err != nil {
		return err
	}
	return os.RemoveAll(tmp)
}

// Map returns a list of all images in the graph, addressable by ID.
func (graph *Graph) Map() (map[string]*Image, error) {
	// FIXME: this should replace All()
	all, err := graph.All()
	if err != nil {
		return nil, err
	}
	images := make(map[string]*Image, len(all))
	for _, image := range all {
		images[image.ID] = image
	}
	return images, nil
}

// All returns a list of all images in the graph.
func (graph *Graph) All() ([]*Image, error) {
	var images []*Image
	err := graph.WalkAll(func(image *Image) {
		images = append(images, image)
	})
	return images, err
}

// WalkAll iterates over each image in the graph, and passes it to a handler.
// The walking order is undetermined.
func (graph *Graph) WalkAll(handler func(*Image)) error {
	files, err := ioutil.ReadDir(graph.Root)
	if err != nil {
		return err
	}
	for _, st := range files {
		if img, err := graph.Get(st.Name()); err != nil {
			// Skip image
			continue
		} else if handler != nil {
			handler(img)
		}
	}
	return nil
}

// ByParent returns a lookup table of images by their parent.
// If an image of id ID has 3 children images, then the value for key ID
// will be a list of 3 images.
// If an image has no children, it will not have an entry in the table.
func (graph *Graph) ByParent() (map[string][]*Image, error) {
	byParent := make(map[string][]*Image)
	err := graph.WalkAll(func(image *Image) {
		parent, err := graph.Get(image.Parent)
		if err != nil {
			return
		}
		if children, exists := byParent[parent.ID]; exists {
			byParent[parent.ID] = append(children, image)
		} else {
			byParent[parent.ID] = []*Image{image}
		}
	})
	return byParent, err
}

// Heads returns all heads in the graph, keyed by id.
// A head is an image which is not the parent of another image in the graph.
func (graph *Graph) Heads() (map[string]*Image, error) {
	heads := make(map[string]*Image)
	byParent, err := graph.ByParent()
	if err != nil {
		return nil, err
	}
	err = graph.WalkAll(func(image *Image) {
		// If it's not in the byParent lookup table, then
		// it's not a parent -> so it's a head!
		if _, exists := byParent[image.ID]; !exists {
			heads[image.ID] = image
		}
	})
	return heads, err
}

func (graph *Graph) imageRoot(id string) string {
	return path.Join(graph.Root, id)
}

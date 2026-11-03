//go:build !linux

package mem

// Read no está implementado fuera de Linux: la interfaz oculta el panel.
func Read() (Snapshot, error) {
	return Snapshot{}, ErrUnsupported
}

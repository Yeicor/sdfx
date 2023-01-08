//-----------------------------------------------------------------------------
/*

Dual Contouring

Convert an SDF3 to a triangle mesh.
Supports sharp edges.
Based on: https://github.com/emilk/Dual-Contouring/

*/
//-----------------------------------------------------------------------------

package dc

import (
	"fmt"
	"log"
	"math"

	"github.com/deadsy/sdfx/render"
	"github.com/deadsy/sdfx/sdf"
	"github.com/deadsy/sdfx/vec/conv"
	"github.com/deadsy/sdfx/vec/v2i"
	v3 "github.com/deadsy/sdfx/vec/v3"
	"github.com/deadsy/sdfx/vec/v3i"
)

//-----------------------------------------------------------------------------
// PUBLIC INTERFACE
//-----------------------------------------------------------------------------

// DualContouringV2 renders a SDF using dual contouring (sharp edges!).
// You may need to test several settings to find the best for your purposes.
type DualContouringV2 struct {
	meshCells int // number of cells on the longest axis of bounding box. e.g 200
	// FarAway fixes bad triangles that may be generated by limiting the maximum vertex displacement
	// from the voxel's center to the specified amount (manhattan distance), and clamping if exceeded.
	FarAway float64
	// CenterPush may generate a better mesh if larger at the cost of less sharp edges.
	CenterPush float64

	// see sdf.Raycast3
	RaycastScaleAndSigmoid, RaycastStepScale, RaycastEpsilon float64
	// see sdf.Raycast3
	RaycastMaxSteps int

	// Warnings printed to screen
	maxCornerDistWarned      bool
	qefFailedImplWarned      bool
	qefFailedWarned          bool
	farAwayWarned            bool
	faceVertexNotFoundWarned bool
	raycastFailedWarned      bool
}

// NewDualContouringDefault uses somewhat safe defaults that sacrifice performance, you may reduce max steps and fix other parameters if facing errors
func NewDualContouringDefault(meshCells int) *DualContouringV2 {
	return NewDualContouringV2(0.499999, 0.01, 0, 1, 1e-4, 1000, meshCells)
}

// NewDualContouringV2 see DualContouringV2 and its fields
func NewDualContouringV2(farAway float64, centerPush float64, raycastScaleAndSigmoid, raycastStepSize float64, raycastEpsilon float64, raycastMaxSteps int, meshCells int) *DualContouringV2 {
	return &DualContouringV2{
		meshCells:              meshCells,
		FarAway:                farAway,
		CenterPush:             centerPush,
		RaycastScaleAndSigmoid: raycastScaleAndSigmoid,
		RaycastStepScale:       raycastStepSize,
		RaycastEpsilon:         raycastEpsilon,
		RaycastMaxSteps:        raycastMaxSteps,
	}
}

// Info returns a string describing the rendered volume.
func (dc *DualContouringV2) Info(s sdf.SDF3) string {
	resolution, cells := dc.getCells(s)
	return fmt.Sprintf("%dx%dx%d, resolution %.2f", cells.X, cells.Y, cells.Z, resolution)
}

// Render produces a 3d triangle mesh over the bounding volume of an sdf3.
func (dc *DualContouringV2) Render(s sdf.SDF3, meshCells int, output chan<- *render.Triangle3) {
	// Place one vertex for each cellIndex
	_, cells := dc.getCells(s)
	s2 := &dcSdf{s, map[v3.Vec]float64{}}
	vertexBuffer, vertexVoxelInfo, vertexVoxelInfoIndexed := dc.placeVertices(s2, cells)
	// Stitch vertices together generating triangles
	dc.generateTriangles(s2, vertexBuffer, vertexVoxelInfo, vertexVoxelInfoIndexed, output)
}

func (dc *DualContouringV2) getCells(s sdf.SDF3) (float64, v3i.Vec) {
	bbSize := s.BoundingBox().Size()
	resolution := bbSize.MaxComponent() / float64(dc.meshCells)
	return resolution, conv.V3ToV3i(bbSize.DivScalar(resolution))
}

//-----------------------------------------------------------------------------
// SDF modifier & cache
//-----------------------------------------------------------------------------

type dcSdf struct {
	impl  sdf.SDF3
	cache map[v3.Vec]float64
}

func (d *dcSdf) evaluateCached(p v3.Vec) float64 { // Reduces evaluation cost from 62.8% to 46.3% on cylinder_head
	res, ok := d.cache[p]
	if ok {
		return res
	}
	res = d.Evaluate(p)
	d.cache[p] = res
	return res
}

func (d *dcSdf) Evaluate(p v3.Vec) float64 {
	return d.impl.Evaluate(p)
}

func (d *dcSdf) BoundingBox() sdf.Box3 {
	bb := d.impl.BoundingBox()
	bb.Max = bb.Max.AddScalar(1e-12) // Just in case borders are 0
	return bb
}

//-----------------------------------------------------------------------------
// CONSTANT DATA
//-----------------------------------------------------------------------------

var dcCorners = []v3.Vec{
	{0, 0, 0}, {0, 0, 1}, {0, 1, 0}, {0, 1, 1},
	{1, 0, 0}, {1, 0, 1}, {1, 1, 0}, {1, 1, 1},
}

var dcEdges = []v2i.Vec{
	{0, 1}, {0, 2}, {0, 4},
	{1, 3}, {1, 5},
	{2, 3}, {2, 6},
	{3, 7},
	{4, 5}, {4, 6},
	{5, 7},
	{6, 7},
}

var dcFarEdges = []v2i.Vec{
	{3, 7},
	{5, 7},
	{6, 7},
}

var dcAxes = []v3.Vec{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}

//-----------------------------------------------------------------------------
// MAIN ALGORITHM
//-----------------------------------------------------------------------------

type dcVoxelInfo struct {
	cellIndex v3i.Vec
	bufIndex  int
	// Cached metadata (could be removed if memory is a problem)
	cellStart, cellSize v3.Vec
}

func (dc *DualContouringV2) placeVertices(s *dcSdf, cells v3i.Vec) (buf []v3.Vec, bufMap []*dcVoxelInfo, bufMapIndexed map[v3i.Vec]*dcVoxelInfo) {
	// Start with big enough buffers for performance avoiding allocations (but not too big, may expand later)
	buf = make([]v3.Vec, 0, dcMaxI(32, cells.X*cells.Y*cells.Z/100))
	bufMap = make([]*dcVoxelInfo, 0, dcMaxI(32, cells.X*cells.Y*cells.Z/100))
	bufMapIndexed = make(map[v3i.Vec]*dcVoxelInfo, dcMaxI(32, cells.X*cells.Y*cells.Z/100))
	// Other pre-allocated vertex placing buffers
	normals := make([]v3.Vec, 0, 11)
	planeDs := make([]float64, 0, 11)
	// Some cached variables
	bb := s.BoundingBox()
	cellSize := bb.Size().Div(conv.V3iToV3(cells))
	cellSizeHalf := cellSize.DivScalar(2)
	cellIndex := v3i.Vec{}
	// Iterate over all cells (could be parallelized, synchronizing on each vertex positioned)
	for cellIndex.X = 0; cellIndex.X < cells.X; cellIndex.X++ {
		for cellIndex.Y = 0; cellIndex.Y < cells.Y; cellIndex.Y++ {
			for cellIndex.Z = 0; cellIndex.Z < cells.Z; cellIndex.Z++ {
				// Generate each vertex (if the surface crosses the voxel)
				cellStart := bb.Min.Add(cellSize.Mul(conv.V3iToV3(cellIndex)))
				cellCenter := cellStart.Add(cellSizeHalf)
				vertexPos := dc.placeVertex(s, cellStart, cellCenter, cellSize, normals[:0], planeDs[:0])
				if !math.IsInf(vertexPos.X, 0) {
					bufIndex := len(buf)
					buf = append(buf, vertexPos)
					info := &dcVoxelInfo{
						cellIndex: cellIndex,
						bufIndex:  bufIndex,
						cellStart: cellStart,
						cellSize:  cellSize,
					}
					bufMapIndexed[cellIndex] = info
					bufMap = append(bufMap, info)
					bufIndex++
				}
			}
		}
	}
	return
}

func (dc *DualContouringV2) placeVertex(s *dcSdf, cellStart, cellCenter, cellSize v3.Vec, normals []v3.Vec, planeDs []float64) v3.Vec {
	inside := dc.computeCornersInside(s, cellStart, cellSize)
	if inside == 0 || inside == math.MaxUint8 {
		// voxel is fully inside or outside the volume: no vertex to place
		return v3.Vec{X: math.Inf(1)}
	}

	//// Add candidate planes from all surface-crossing edges (using the surface point on the edge)
	for _, edge := range dcEdges { // Use edges instead of corners to generate less positions and normals.
		if ((inside >> edge.X) & 1) == ((inside >> edge.Y) & 1) { // Not crossing edge
			continue
		}
		//crossingCorners = crossingCorners | (1 << edge[0]) | (1 << edge[1])
		cornerPos1 := cellStart.Add(dcCorners[edge.X].Mul(cellSize))
		cornerPos2 := cellStart.Add(dcCorners[edge.Y].Mul(cellSize))
		//edgeSurfPos := dcApproximateZeroCrossingPosition(s, cornerPos1, cornerPos2)
		dir := cornerPos2.Sub(cornerPos1)
		dirLength := dir.Length()
		edgeSurfPos, t, steps := sdf.Raycast3(s, cornerPos1, dir, dc.RaycastScaleAndSigmoid, dc.RaycastStepScale,
			dc.RaycastEpsilon, dirLength*2, dc.RaycastMaxSteps)
		if t < 0 || t > dirLength {
			if !dc.raycastFailedWarned {
				log.Println("[DualContouringV1] WARNING: raycast failed (steps:", steps, "- try modifying options), using fallback low accuracy implementation")
				dc.raycastFailedWarned = true
			}
			edgeSurfPos = dcApproximateZeroCrossingPosition(s, cornerPos1, cornerPos2)
		}
		edgeSurfNormal := sdf.Normal3(s, edgeSurfPos, 1e-3)
		normals = append(normals, edgeSurfNormal)
		planeDs = append(planeDs, edgeSurfNormal.Dot(edgeSurfPos) /* - s.Evaluate(edgeSurfPos): 0.0 */)
		if len(normals) == 6 {
			break // There cannot be more than 6 crossed edges...
		}
	}

	/*
	 Add a weak 'push' towards the voxel center to improve conditioning.
	 This is needed for any surface which is flat in at least one dimension, including a cylinder.
	 We could do only as needed (when lastSquared have failed once),
	 but the push is so weak that it makes little difference to the precision of the model.
	*/
	for _, axis := range dcAxes {
		normal := axis.MulScalar(dc.CenterPush)
		//positions = append(positions, cellCenter)
		planeDs = append(planeDs, normal.Dot(cellCenter))
		normals = append(normals, normal)
	}

	// Now actually compute the vertex from all planes (corner normals and planeDs) collected
	vertexPos := dc.computeVertexPos(normals, planeDs)

	// Check if vertex positioning failed
	if math.IsInf(vertexPos.X, 0) {
		if !dc.qefFailedWarned {
			log.Println("[DualContouringV1] WARNING: vertex positioning failed, centering vertex position!")
			dc.qefFailedWarned = true
		}
		vertexPos = cellCenter
	}

	// Check if vertex was generated too far away and will probably generate bad triangles
	if math.Abs(vertexPos.X-cellCenter.X) > dc.FarAway*cellSize.X || // Using manhattan distance (0.5 equals in the same voxel)
		math.Abs(vertexPos.Y-cellCenter.Y) > dc.FarAway*cellSize.Y ||
		math.Abs(vertexPos.Z-cellCenter.Z) > dc.FarAway*cellSize.Z {
		if !dc.farAwayWarned {
			log.Print("[DualContouringV1] WARNING: generated a vertex two far away from voxel (by ",
				vertexPos.Sub(cellCenter), ", from ", cellCenter, " to ", vertexPos, "), clamping vertex position!\n")
			dc.farAwayWarned = true
		}
		vertexPos = vertexPos.Clamp(cellStart, cellStart.Add(cellSize)) // Just clamp
	}

	return vertexPos
}

func (dc *DualContouringV2) computeCornersInside(s *dcSdf, cellStart v3.Vec, cellSize v3.Vec) uint8 {
	// Check each corner and store if they are inside or outside the surface in the bit set
	inside := uint8(0)
	for i, corner := range dcCorners {
		isSolid := s.evaluateCached(cellStart.Add(corner.Mul(cellSize))) < 0
		if isSolid {
			inside = inside | (1 << i)
		}
	}
	return inside
}

func (dc *DualContouringV2) generateTriangles(s *dcSdf, vertices []v3.Vec, info []*dcVoxelInfo, infoI map[v3i.Vec]*dcVoxelInfo, output chan<- *render.Triangle3) {
	for _, voxelInfo := range info {
		k0 := voxelInfo.bufIndex // k0 is the vertex (index) of this voxel, which will be connected to others
		cellIndex := voxelInfo.cellIndex

		inside := dc.computeCornersInside(s, voxelInfo.cellStart, voxelInfo.cellSize)

		// Connect to triangles in the 3 main axes (two triangles each, if crossing the surface)
		for ai := 0; ai < 3; ai++ {
			edge := dcFarEdges[ai]
			if ((inside >> edge.X) & 1) == ((inside >> edge.Y) & 1) {
				continue // Not a crossing
			}

			// Get other vertices for triangle generation
			var k1, k2, k3 *dcVoxelInfo
			if ai == 0 {
				k1, _ = infoI[cellIndex.Add(v3i.Vec{0, 0, 1})]
				k2, _ = infoI[cellIndex.Add(v3i.Vec{0, 1, 0})]
				k3, _ = infoI[cellIndex.Add(v3i.Vec{0, 1, 1})]
			} else if ai == 1 {
				k1, _ = infoI[cellIndex.Add(v3i.Vec{0, 0, 1})]
				k2, _ = infoI[cellIndex.Add(v3i.Vec{1, 0, 0})]
				k3, _ = infoI[cellIndex.Add(v3i.Vec{1, 0, 1})]
			} else {
				k1, _ = infoI[cellIndex.Add(v3i.Vec{0, 1, 0})]
				k2, _ = infoI[cellIndex.Add(v3i.Vec{1, 0, 0})]
				k3, _ = infoI[cellIndex.Add(v3i.Vec{1, 1, 0})]
			}

			if k1 == nil || k2 == nil || k3 == nil { // Shouldn't ever happen
				if !dc.faceVertexNotFoundWarned {
					log.Println("[DualContouringV1] WARNING: no vertex found for completing face, there will be holes")
					dc.faceVertexNotFoundWarned = true
				}
				continue
			}

			// Define triangles
			t0 := &render.Triangle3{V: [3]v3.Vec{vertices[k0], vertices[k1.bufIndex], vertices[k3.bufIndex]}}
			t1 := &render.Triangle3{V: [3]v3.Vec{vertices[k0], vertices[k3.bufIndex], vertices[k2.bufIndex]}}

			// Get the normals right:
			if ((inside >> edge.X) & 1) != uint8(ai&1) { // xor
				t0 = dcFlip(t0)
				t1 = dcFlip(t1)
			}

			// Output built triangles (if not degenerate)
			if !t0.Degenerate(0) {
				output <- t0
			}
			if !t1.Degenerate(0) {
				output <- t1
			}
		}
	}
}

//-----------------------------------------------------------------------------
// VERTEX POSITION SOLVER
//-----------------------------------------------------------------------------

func (dc *DualContouringV2) computeVertexPos(normals []v3.Vec, planeDs []float64) v3.Vec {
	// ### 1. Minecraft-like voxels
	//return cellCenter
	// ### 2. Solve using least squares
	return dc.leastSquares(normals, planeDs)
	// ### 3. Solve using least squares (gonum)
	//A := mat.NewDense(len(normals), 3, nil)
	//b := mat.NewVecDense(len(planeDs), nil)
	//for row, normal := range normals {
	//	A.Set(row, 0, normal.X)
	//	A.Set(row, 1, normal.Y)
	//	A.Set(row, 2, normal.Z)
	//	b.SetVec(row, planeDs[row])
	//}
	//res := &mat.Dense{}
	//err := res.Solve(A, b)
	//if err != nil {
	//	if !dc.qefFailedImplWarned {
	//		log.Println("[DualContouringV1] WARNING: QEF solver failed: ", err.Error())
	//		dc.qefFailedImplWarned = true
	//	}
	//	return v3.Vec{X: math.Inf(1)}
	//}
	//return v3.Vec{X: res.At(0, 0), Y: res.At(1, 0), Z: res.At(2, 0)}
}

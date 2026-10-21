//! Versioned section palettes for the C interface, independent of registry numbering.
use rustc_hash::FxHashMap as HashMap;
use std::sync::LazyLock;
use steel_core::chunk::paletted_container::PalettedContainer;
use steel_registry::blocks::block_state_ext::BlockStateExt;
use steel_registry::{REGISTRY, RegistryExt};
use steel_utils::BlockStateId;

pub(crate) static DESCRIPTORS: LazyLock<Vec<Vec<u8>>> = LazyLock::new(|| {
    let mut descriptors = Vec::new();
    while let Ok(index) = u16::try_from(descriptors.len()) {
        let id = BlockStateId(index);
        if REGISTRY.blocks.by_state_id(id).is_none() {
            break;
        }
        let mut descriptor = Vec::new();
        string(&mut descriptor, &state_key(id));
        string(&mut descriptor, &state_key(id.get_block().default_state()));
        let water = steel_registry::fluid::is_water_fluid(id.get_fluid_state().fluid_id)
            && id.get_block().key != steel_registry::vanilla_blocks::WATER.key;
        descriptor.push(u8::from(id.is_solid()) | (u8::from(water) << 1));
        descriptors.push(descriptor);
    }
    descriptors
});

/// Writes a length-prefixed UTF-8 string to the bridge stream.
fn string(out: &mut Vec<u8>, value: &str) {
    let len = u16::try_from(value.len()).expect("state name too long");
    out.extend_from_slice(&len.to_le_bytes());
    out.extend_from_slice(value.as_bytes());
}

/// Formats a Java state by name and sorted properties, independently of registry numbering.
fn state_key(id: BlockStateId) -> String {
    let block = id.get_block();
    let mut key = block.key.to_string();
    let mut properties = REGISTRY.blocks.get_properties(id);
    properties.sort_unstable();
    if !properties.is_empty() {
        key.push('[');
        key.push_str(
            &properties
                .into_iter()
                .map(|(k, v)| format!("{k}={v}"))
                .collect::<Vec<_>>()
                .join(","),
        );
        key.push(']');
    }
    key
}

/// Exports section palettes, x-fastest block indices and 4×4×4 biome indices.
pub(crate) fn encode(chunk: &steel_core::chunk::Chunk) -> Vec<u8> {
    let mut out = Vec::with_capacity(210_000);
    out.extend_from_slice(b"SWG1");
    out.extend_from_slice(&(-64_i16).to_le_bytes());
    out.extend_from_slice(&24_u16.to_le_bytes());
    for holder in &chunk.sections().sections {
        let section = holder.read();
        let (palette, indices) = if let PalettedContainer::Homogeneous(id) = &section.states {
            (vec![*id], vec![0_u16; 4096])
        } else {
            let mut palette = Vec::new();
            let mut lookup = HashMap::default();
            let mut indices = Vec::with_capacity(4096);
            for y in 0..16 {
                for z in 0..16 {
                    for x in 0..16 {
                        let id = section.states.get(x, y, z);
                        let n = *lookup.entry(id.0).or_insert_with(|| {
                            palette.push(id);
                            (palette.len() - 1) as u16
                        });
                        indices.push(n);
                    }
                }
            }
            (palette, indices)
        };
        out.extend_from_slice(&(palette.len() as u16).to_le_bytes());
        for id in palette {
            out.extend_from_slice(&DESCRIPTORS[usize::from(id.0)]);
        }
        for index in indices {
            out.extend_from_slice(&index.to_le_bytes());
        }
        let mut palette = Vec::new();
        let mut lookup = HashMap::default();
        let mut indices = Vec::with_capacity(64);
        for y in 0..4 {
            for z in 0..4 {
                for x in 0..4 {
                    let id = section.biomes.get(x, y, z);
                    let n = *lookup.entry(id).or_insert_with(|| {
                        palette.push(id);
                        (palette.len() - 1) as u16
                    });
                    indices.push(n);
                }
            }
        }
        out.extend_from_slice(&(palette.len() as u16).to_le_bytes());
        for id in palette {
            string(
                &mut out,
                &REGISTRY
                    .biomes
                    .by_id(usize::from(id))
                    .expect("biome")
                    .key
                    .to_string(),
            );
        }
        for index in indices {
            out.extend_from_slice(&index.to_le_bytes());
        }
    }
    out
}

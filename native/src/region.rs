//! Canonical generation regions keep block output independent of request order.
use crate::encoding::encode;
use crate::host::{close_world, open_world, panic_message};
use futures::FutureExt;
use rayon::prelude::*;
use std::panic::AssertUnwindSafe;
use std::sync::Arc;
use steel_core::chunk::status::ChunkStatus;
use steel_core::entity::damage::DamageHistory;
use steel_core::world::World;
use steel_core::worldgen::ChunkGeneratorType;
use steel_utils::ChunkPos;

pub(crate) const WIDTH: i32 = 128;
pub(crate) type Outputs = Vec<Arc<[u8]>>;

/// Generates one fixed X-aligned region, including every upstream dependency.
pub(crate) async fn generate(
    seed: i64,
    key: (i32, i32),
    generator: Arc<ChunkGeneratorType>,
    damage: &Arc<DamageHistory>,
    pool: &Arc<rayon::ThreadPool>,
) -> Result<Outputs, String> {
    let world = open_world(seed, generator, damage, pool).await;
    world.chunk_map.stop_generation_refill_loop();
    world.chunk_map.task_tracker.close();
    world.chunk_map.task_tracker.wait().await;
    let result = AssertUnwindSafe(fill(&world, key, pool))
        .catch_unwind()
        .await
        .map_err(panic_message)
        .and_then(|result| result);
    close_world(&world).await;
    result
}

/// Runs each upstream stage once over its complete input halo, then encodes the core.
async fn fill(
    world: &Arc<World>,
    key: (i32, i32),
    pool: &Arc<rayon::ThreadPool>,
) -> Result<Outputs, String> {
    let start = key.0 * WIDTH;
    let holders = world
        .chunk_map
        .generate_region(
            ChunkPos::new(start, key.1),
            ChunkPos::new(start + WIDTH - 1, key.1),
        )
        .await
        .map_err(|error| error.to_string())?;
    pool.install(|| {
        holders
            .par_iter()
            .map(|holder| {
                let Some(chunk) = holder.try_chunk(ChunkStatus::Full) else {
                    return Err("Full chunk disappeared before encoding".to_owned());
                };
                Ok(Arc::<[u8]>::from(encode(chunk)))
            })
            .collect()
    })
}

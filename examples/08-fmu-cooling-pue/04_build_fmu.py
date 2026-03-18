#!/usr/bin/env python3
"""Build an FMU from a Modelica data center cooling model using Docker.

Uses the official OpenModelica Docker image to compile a Modelica model into
an FMI 2.0 co-simulation FMU. No local OpenModelica installation required.

The default model is a simplified wrapper that exposes IT heat load and outdoor
temperature as inputs and returns cooling electrical power. Replace the
equation section (or the entire .mo file) with the full LBL Buildings library
DXCooled model for physics-based simulation.

Prerequisites:
    - Docker installed and running
    - (Optional) pip install fmpy  — for running the FMU afterwards

Usage:
    python 04_build_fmu.py                              # build with Docker
    python 04_build_fmu.py --check                      # check prerequisites
    python 04_build_fmu.py --model-file my_model.mo     # build custom .mo file
    python 04_build_fmu.py --install-buildings           # install LBL Buildings lib first
    python 04_build_fmu.py --local                       # use local omc instead of Docker
"""
import argparse
import json
import os
import pathlib
import shutil
import subprocess
import sys
import tempfile

SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
FMU_OUT_DIR = SCRIPT_DIR / "cooling_models"

DOCKER_IMAGE = "openmodelica/openmodelica:v1.26.3-ompython"
WRAPPER_MODEL_NAME = "DataCenterCoolingWrapper"
DEFAULT_MODEL_FILE = FMU_OUT_DIR / f"{WRAPPER_MODEL_NAME}.mo"


def check_docker() -> bool:
    """Check if Docker is available and the image exists or can be pulled."""
    docker = shutil.which("docker")
    if not docker:
        print("  Docker: NOT FOUND")
        return False
    try:
        subprocess.run(["docker", "info"], capture_output=True, timeout=10, check=True)
    except (subprocess.CalledProcessError, subprocess.TimeoutExpired):
        print("  Docker: found but daemon not running")
        return False
    # Check if image is already pulled
    result = subprocess.run(
        ["docker", "images", "-q", DOCKER_IMAGE],
        capture_output=True, text=True, timeout=10,
    )
    if result.stdout.strip():
        print(f"  Docker: OK (image {DOCKER_IMAGE} available)")
    else:
        print(f"  Docker: OK (image {DOCKER_IMAGE} will be pulled on first build)")
    return True


def check_local_omc() -> tuple[bool, bool]:
    """Check local omc and Buildings library. Returns (has_omc, has_buildings)."""
    omc = shutil.which("omc")
    if not omc:
        print("  Local omc: NOT FOUND")
        return False, False
    try:
        version = subprocess.run(
            ["omc", "--version"], capture_output=True, text=True, timeout=10
        ).stdout.strip()
        print(f"  Local omc: {version}")
    except Exception:
        print("  Local omc: found but failed to run")
        return False, False
    # Check Buildings
    try:
        result = subprocess.run(
            ["omc", "-e", 'loadModel(Buildings); getErrorString();'],
            capture_output=True, text=True, timeout=30,
        )
        has_buildings = "Error" not in result.stdout
        print(f"  Buildings library: {'installed' if has_buildings else 'NOT FOUND'}")
        return True, has_buildings
    except Exception:
        return True, False


def check_fmpy() -> bool:
    try:
        import fmpy
        print(f"  fmpy: {fmpy.__version__}")
        return True
    except ImportError:
        print("  fmpy: NOT FOUND (pip install fmpy)")
        return False


def write_build_context(build_dir: pathlib.Path, model_name: str, model_code: str,
                        install_buildings: bool) -> pathlib.Path:
    """Write the .mo file and .mos build script to build_dir."""
    # Write model file
    mo_path = build_dir / f"{model_name}.mo"
    mo_path.write_text(model_code)

    # Write OMC build script
    mos_lines = []
    mos_lines.append('installPackage(Modelica);')
    mos_lines.append('getErrorString();')
    mos_lines.append('loadModel(Modelica);')
    mos_lines.append('getErrorString();')
    if install_buildings:
        mos_lines.append('installPackage(Buildings, "12.1.0");')
        mos_lines.append('getErrorString();')
        mos_lines.append('loadModel(Buildings);')
        mos_lines.append('getErrorString();')
    mos_lines.append(f'loadFile("{model_name}.mo");')
    mos_lines.append('getErrorString();')
    mos_lines.append(
        f'buildModelFMU({model_name}, version="2.0", fmuType="cs");'
    )
    mos_lines.append('getErrorString();')

    mos_path = build_dir / "build.mos"
    mos_path.write_text("\n".join(mos_lines) + "\n")
    return mos_path


def build_with_docker(model_name: str, model_code: str,
                      install_buildings: bool) -> pathlib.Path:
    """Build FMU using Docker container."""
    FMU_OUT_DIR.mkdir(parents=True, exist_ok=True)

    with tempfile.TemporaryDirectory(prefix="joulie-fmu-build-") as tmpdir:
        build_dir = pathlib.Path(tmpdir)
        write_build_context(build_dir, model_name, model_code, install_buildings)

        print(f"  build context: {build_dir}")
        print(f"  docker image:  {DOCKER_IMAGE}")
        print(f"  compiling {model_name} -> FMU ...")

        # Run omc inside the container, mounting the build directory
        result = subprocess.run(
            [
                "docker", "run", "--rm",
                "-v", f"{build_dir}:/work",
                "-w", "/work",
                DOCKER_IMAGE,
                "omc", "build.mos",
            ],
            capture_output=True, text=True, timeout=600,
        )

        stdout = result.stdout.strip()
        stderr = result.stderr.strip()
        if stdout:
            print(f"  omc stdout: {stdout[:800]}")
        if stderr:
            print(f"  omc stderr: {stderr[:800]}")

        # Find the generated FMU
        fmu_files = list(build_dir.glob("*.fmu"))
        if not fmu_files:
            print(f"\nERROR: FMU compilation failed. No .fmu file produced.", file=sys.stderr)
            # Show all files for debugging
            all_files = list(build_dir.iterdir())
            print(f"  files in build dir: {[f.name for f in all_files]}", file=sys.stderr)
            sys.exit(1)

        fmu_src = fmu_files[0]
        fmu_dst = FMU_OUT_DIR / f"{model_name}.fmu"
        shutil.copy2(fmu_src, fmu_dst)
        size_kb = fmu_dst.stat().st_size / 1024
        print(f"  FMU built: {fmu_dst} ({size_kb:.0f} KB)")
        return fmu_dst


def build_with_local_omc(model_name: str, model_code: str,
                         install_buildings: bool) -> pathlib.Path:
    """Build FMU using local omc installation."""
    FMU_OUT_DIR.mkdir(parents=True, exist_ok=True)

    with tempfile.TemporaryDirectory(prefix="joulie-fmu-build-") as tmpdir:
        build_dir = pathlib.Path(tmpdir)
        mos_path = write_build_context(build_dir, model_name, model_code, install_buildings)

        print(f"  compiling {model_name} -> FMU (local omc) ...")
        result = subprocess.run(
            ["omc", str(mos_path)],
            capture_output=True, text=True, timeout=300,
            cwd=str(build_dir),
        )

        if result.stdout.strip():
            print(f"  omc stdout: {result.stdout[:800]}")
        if result.stderr.strip():
            print(f"  omc stderr: {result.stderr[:800]}")

        fmu_files = list(build_dir.glob("*.fmu"))
        if not fmu_files:
            print(f"\nERROR: FMU compilation failed.", file=sys.stderr)
            sys.exit(1)

        fmu_dst = FMU_OUT_DIR / f"{model_name}.fmu"
        shutil.copy2(fmu_files[0], fmu_dst)
        print(f"  FMU built: {fmu_dst} ({fmu_dst.stat().st_size / 1024:.0f} KB)")
        return fmu_dst


def main():
    ap = argparse.ArgumentParser(description="Build FMU from Modelica cooling model")
    ap.add_argument("--model", default=WRAPPER_MODEL_NAME,
                    help="Modelica model name to compile")
    ap.add_argument("--model-file", default="",
                    help="Path to custom .mo file (overrides built-in wrapper)")
    ap.add_argument("--check", action="store_true",
                    help="Only check prerequisites, don't build")
    ap.add_argument("--local", action="store_true",
                    help="Use local omc instead of Docker")
    ap.add_argument("--install-buildings", action="store_true",
                    help="Install LBL Buildings library inside the container before building")
    args = ap.parse_args()

    print("=== Checking prerequisites ===")
    has_docker = check_docker()
    has_omc, has_buildings = check_local_omc()
    has_fmpy = check_fmpy()

    if args.check:
        print("\n=== Summary ===")
        can_build = has_docker or has_omc
        print(f"  Can build FMU (Docker): {'YES' if has_docker else 'NO'}")
        print(f"  Can build FMU (local):  {'YES' if has_omc else 'NO'}")
        print(f"  Can run FMU (fmpy):     {'YES' if has_fmpy else 'NO'}")
        print(f"  Buildings library:      {'YES (local)' if has_buildings else 'use --install-buildings flag'}")
        sys.exit(0 if can_build else 1)

    # Determine model source
    model_name = args.model
    if args.model_file:
        mo_path = pathlib.Path(args.model_file).resolve()
        if not mo_path.exists():
            print(f"Model file not found: {mo_path}", file=sys.stderr)
            sys.exit(1)
        model_code = mo_path.read_text()
        # Infer model name from file
        model_name = mo_path.stem
        print(f"\n=== Building FMU from {mo_path.name} ===")
    else:
        if not DEFAULT_MODEL_FILE.exists():
            print(f"Default model file not found: {DEFAULT_MODEL_FILE}", file=sys.stderr)
            print("Provide --model-file or place DataCenterCoolingWrapper.mo in cooling_models/", file=sys.stderr)
            sys.exit(1)
        model_code = DEFAULT_MODEL_FILE.read_text()
        print(f"\n=== Building FMU: {model_name} (from {DEFAULT_MODEL_FILE.name}) ===")

    # Build
    if args.local:
        if not has_omc:
            print("Local omc not found. Use Docker (default) or install OpenModelica.", file=sys.stderr)
            sys.exit(1)
        fmu_path = build_with_local_omc(model_name, model_code, args.install_buildings)
    else:
        if not has_docker:
            if has_omc:
                print("  Docker not available, falling back to local omc")
                fmu_path = build_with_local_omc(model_name, model_code, args.install_buildings)
            else:
                print("Neither Docker nor local omc available.", file=sys.stderr)
                print(f"  Install Docker, or install OpenModelica locally.", file=sys.stderr)
                sys.exit(1)
        else:
            fmu_path = build_with_docker(model_name, model_code, args.install_buildings)

    print(f"\n=== Done ===")
    print(f"FMU: {fmu_path}")
    print(f"\nNext steps:")
    print(f"  python 02_apply_cooling_models.py --fmu {fmu_path}")
    print(f"  python 03_plot_pue_comparison.py")


if __name__ == "__main__":
    main()

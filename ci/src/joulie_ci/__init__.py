"""Dagger module of Joulie's CI.

Every job a pull request runs is a function of this module, and the GitHub
workflows run them with `dagger call`, so `dagger -m ./ci call <function>`
runs on a laptop exactly what CI runs: the same images, pinned by digest, the
same tools and the same commands. `all` runs every check at once.
"""

from .main import JoulieCi as JoulieCi

__all__ = ["JoulieCi"]

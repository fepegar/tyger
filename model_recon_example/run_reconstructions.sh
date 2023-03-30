#!/bin/bash

this_folder="$(dirname "$0")"
eminence_script="$(find "${this_folder}/../" -name eminence.py)"

rm "${this_folder}/*out.h5"
time python "$eminence_script" -f "${this_folder}/fastmri_brain_undersampled.h5" -o "${this_folder}/standard_recon_out.h5" -r "${this_folder}/standard_recon.yml"
time python "$eminence_script" -f "${this_folder}/fastmri_brain_undersampled.h5" -o "${this_folder}/model_recon_cpu_out.h5" -r "${this_folder}/model_recon_cpu.yml"
time python "$eminence_script" -f "${this_folder}/fastmri_brain_undersampled.h5" -o "${this_folder}/model_recon_out.h5" -r "${this_folder}/model_recon.yml"

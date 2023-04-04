import argparse
import ismrmrd
import scipy.io
import numpy as np


def convert_physiomri(filename, output):
    raw = scipy.io.loadmat(filename)
    raw_data = raw['dataFull']

    larmor_f = raw['larmorFreq'].flatten()[0]
    repetitions = raw['nScans'].flatten()[0]

    ro_length = raw_data.shape[-1]
    lines = raw_data.shape[-2]
    partitions = raw_data.shape[-3]

    header = ismrmrd.xsd.ismrmrdHeader()
    exp = ismrmrd.xsd.experimentalConditionsType()
    exp.H1resonanceFrequency_Hz = larmor_f
    header.experimentalConditions = exp

    sys = ismrmrd.xsd.acquisitionSystemInformationType()
    sys.receiverChannels = 1
    header.acquisitionSystemInformation = sys

    # Encoding
    encoding = ismrmrd.xsd.encodingType()
    encoding.trajectory = ismrmrd.xsd.trajectoryType.CARTESIAN

    # encoded and recon spaces
    fov = raw['fov'].flatten()
    efov = ismrmrd.xsd.fieldOfViewMm()
    efov.x = fov[-1]*1000
    efov.y = fov[-2]*1000
    efov.z = fov[-3]*1000
    ematrix = ismrmrd.xsd.matrixSizeType()
    ematrix.x = ro_length
    ematrix.y = lines
    ematrix.z = partitions

    # There does not appear to be any oversampling?
    rfov = efov
    rmatrix = ematrix

    espace = ismrmrd.xsd.encodingSpaceType()
    espace.matrixSize = ematrix
    espace.fieldOfView_mm = efov
    rspace = ismrmrd.xsd.encodingSpaceType()
    rspace.matrixSize = rmatrix
    rspace.fieldOfView_mm = rfov

    # Set encoded and recon spaces
    encoding.encodedSpace = espace
    encoding.reconSpace = rspace

    # Encoding limits
    limits = ismrmrd.xsd.encodingLimitsType()

    limits1 = ismrmrd.xsd.limitType()
    limits1.minimum = 0
    limits1.center = int(ematrix.y/2)
    limits1.maximum = ematrix.y - 1
    limits.kspace_encoding_step_1 = limits1

    limits2 = ismrmrd.xsd.limitType()
    limits2.minimum = 0
    limits2.center = int(ematrix.z/2)
    limits2.maximum = ematrix.z - 1
    limits.kspace_encoding_step_2 = limits2

    replimits = ismrmrd.xsd.limitType()
    replimits.minimum = 0
    replimits.center = 0
    replimits.maximum = repetitions - 1
    limits.repetition = replimits

    limits_rest = ismrmrd.xsd.limitType()
    limits_rest.minimum = 0
    limits_rest.center = 0
    limits_rest.maximum = 0

    limits.kspace_encoding_step_0 = limits_rest
    limits.slice = limits_rest
    limits.average = limits_rest
    limits.contrast = limits_rest
    limits.phase = limits_rest
    limits.segment = limits_rest
    limits.set = limits_rest

    encoding.encodingLimits = limits
    header.encoding.append(encoding)

    dset = ismrmrd.Dataset(output, "dataset", create_if_needed=True)
    dset.write_xml_header(header.toXML('utf-8'))

    # Now we need to create the acquisitions
    for r in range(repetitions):
        print("Repetition %d" % r)
        for z in range(partitions):
            for y in range(lines):
                acq = ismrmrd.Acquisition()
                head = ismrmrd.AcquisitionHeader()
                head.version = 1
                head.number_of_samples = ro_length
                head.active_channels = 1
                head.center_sample = int(ro_length/2)
                head.idx.repetition = r
                head.idx.kspace_encode_step_1 = y
                head.idx.kspace_encode_step_2 = z
                head.sample_time_us = 0
                head.available_channels = 1
                head.discard_pre = 0
                head.discard_post = 0
                acq.setHead(head)

                if y == 0 and z == 0:
                    acq.setFlag(ismrmrd.ACQ_FIRST_IN_REPETITION)
                if y == 0:
                    acq.setFlag(ismrmrd.ACQ_FIRST_IN_ENCODE_STEP1)
                if z == 0:
                    acq.setFlag(ismrmrd.ACQ_FIRST_IN_ENCODE_STEP2)

                if y == raw_data.shape[-2] - 1:
                    acq.setFlag(ismrmrd.ACQ_LAST_IN_ENCODE_STEP1)

                if z == raw_data.shape[-3] - 1:
                    acq.setFlag(ismrmrd.ACQ_LAST_IN_ENCODE_STEP2)

                if (y == raw_data.shape[-2] - 1) and (z == raw_data.shape[-3] - 1):
                    acq.setFlag(ismrmrd.ACQ_LAST_IN_REPETITION)

                acq.resize(raw_data.shape[-1], 1)
                acq.data[:] = raw_data[r, z, y, :]
                dset.append_acquisition(acq)

    dset.close()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description='Convert PhysioMRI data to ISMRMRD')
    parser.add_argument('input', help='Input file')
    parser.add_argument('output', help='Output file')
    args = parser.parse_args()
    convert_physiomri(args.input, args.output)
